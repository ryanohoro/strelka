package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v2"

	grpc_health_v1 "github.com/target/strelka/src/go/api/health"
	"github.com/target/strelka/src/go/api/strelka"
	"github.com/target/strelka/src/go/pkg/rpc"
	"github.com/target/strelka/src/go/pkg/structs"

	"go.opentelemetry.io/otel"
	//	"go.opentelemetry.io/otel/attribute"
	//	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/jaeger"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
)

// Telemetry support
const service_name = "strelka.frontend"

// Type for tracing context
type TextMapCarrier struct {
	data map[string]string
}

var _ propagation.TextMapCarrier = (*TextMapCarrier)(nil)

// NewTextMapCarrier returns a new *TextMapCarrier populated with data.
func NewTextMapCarrier(data map[string]string) *TextMapCarrier {
	copied := make(map[string]string, len(data))
	for k, v := range data {
		copied[k] = v
	}
	return &TextMapCarrier{data: copied}
}

func (c *TextMapCarrier) Keys() []string {
	result := make([]string, 0, len(c.data))
	for k := range c.data {
		result = append(result, k)
	}
	return result
}

func (c *TextMapCarrier) Get(key string) string {
	return c.data[key]
}

func (c *TextMapCarrier) Set(key, value string) {
	fmt.Println("setting")
	fmt.Println(key)
	c.data[key] = value
}

type coord struct {
	cli *redis.Client
}

type gate struct {
	cli *redis.Client
	ttl time.Duration
}

type server struct {
	coordinator coord
	gatekeeper  gate
	responses   chan<- *strelka.ScanResponse
}

type request struct {
	Attributes   *strelka.Attributes `json:"attributes,omitempty"`
	Client       string              `json:"client,omitempty"`
	Id           string              `json:"id,omitempty"`
	Source       string              `json:"source,omitempty"`
	Time         int64               `json:"time,omitempty"`
	Tracecontext string              `json:"tracecontext,omitempty"`
}

func (s *server) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

func (s *server) ScanFile(stream strelka.Frontend_ScanFileServer) error {

	// Collect main context for telemetry
	mainCtx := context.Background()

	// Export traces to Jaeger collector
	exp, err := jaeger.New(jaeger.WithCollectorEndpoint(jaeger.WithEndpoint("http://127.0.0.1:14268/api/traces")))
	if err != nil {
		panic(err)
	}

	// Set up tracing provider
	tp := trace.NewTracerProvider(trace.WithBatcher(exp), trace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(service_name),
		)))

	defer func() {
		if err := tp.Shutdown(mainCtx); err != nil {
			panic(err)
		}
	}()

	otel.SetTracerProvider(tp)

	deadline, ok := stream.Context().Deadline()
	if !ok {
		return nil
	}

	// Hashing for final event (gatekeeper de-duplication)
	hash := sha256.New()

	// Generate a unique Request ID, mark data and event Redis objects
	id := uuid.New().String()
	keyd := fmt.Sprintf("data:%v", id)
	keye := fmt.Sprintf("event:%v", id)

	var attr *strelka.Attributes
	var req *strelka.Request

	// Recieve gRPC data from client
	for {
		in, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if attr == nil {
			attr = in.Attributes
		}
		if req == nil {
			req = in.Request
		}

		hash.Write(in.Data)

		// Send file data to coordinator Redis
		p := s.coordinator.cli.Pipeline()
		p.RPush(stream.Context(), keyd, in.Data)
		p.ExpireAt(stream.Context(), keyd, deadline)
		if _, err := p.Exec(stream.Context()); err != nil {
			return err
		}
	}

	if req == nil || attr == nil {
		return nil
	}
	if req.Id == "" {
		req.Id = id
	}

	sha := fmt.Sprintf("hash:%x", hash.Sum(nil))

	// Embed metadata for request in event
	em := make(map[string]interface{})
	em["request"] = request{
		Attributes:   attr,
		Client:       req.Client,
		Id:           req.Id,
		Source:       req.Source,
		Time:         time.Now().Unix(),
		Tracecontext: req.Tracecontext,
	}

	// Propagate trace context from task in Redis
	tc := map[string]string{"traceparent": req.Tracecontext}
	otel.SetTextMapPropagator(propagation.TraceContext{})
	requestCtx := otel.GetTextMapPropagator().Extract(mainCtx, NewTextMapCarrier(tc))

	// Start tracing
	tracer := otel.Tracer(service_name)
	newCtx, span := tracer.Start(requestCtx, "scan_file")
	defer span.End()
	_ = newCtx // TEMP

	// If the client requests gatekeeper caching support
	if req.Gatekeeper {

		// Check Redis for an event attached to the file hash
		lrange := s.gatekeeper.cli.LRange(stream.Context(), sha, 0, -1).Val()

		// If the gatekeeper has a cached event
		if len(lrange) > 0 {

			for _, e := range lrange {
				// Add cached event data
				if err := json.Unmarshal([]byte(e), &em); err != nil {
					return err
				}

				event, err := json.Marshal(em)
				if err != nil {
					return err
				}

				// Generate a response with cached data
				resp := &strelka.ScanResponse{
					Id:    req.Id,
					Event: string(event),
				}

				// Send gRPC response back to client
				s.responses <- resp
				if err := stream.Send(resp); err != nil {
					return err
				}
			}

			// Delete data from Redis coordinator
			if err := s.coordinator.cli.Del(stream.Context(), keyd).Err(); err != nil {
				return err
			}

			return nil
		}
	}

	requestInfo, err := json.Marshal(em["request"])
	if err != nil {
		return err
	}

	// Add request task to Redis coordinator with expiration timestamp
	span.AddEvent("Add Redis task")
	if err := s.coordinator.cli.ZAdd(
	    stream.Context(),
		"tasks",
		&redis.Z{
			Score:  float64(deadline.Unix()),
			Member: requestInfo,
		},
	).Err(); err != nil {
		return err
	}

	// Delete existing event from gatekeeper cache Redis based on file hash
	tx := s.gatekeeper.cli.TxPipeline()
	tx.Del(stream.Context(), sha)

	for {

		// Wait for event data to appear in the coordinator Redis
		span.AddEvent("Waiting for event...")
		lpop, err := s.coordinator.cli.LPop(stream.Context(), keye).Result()
		if err != nil {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if lpop == "FIN" {
			break
		}

		// Send event to gatekeeper cache
		tx.RPush(stream.Context(), sha, lpop)
		if err := json.Unmarshal([]byte(lpop), &em); err != nil {
			return err
		}

		// Set expiration on chached event
		tx.Expire(stream.Context(), sha, s.gatekeeper.ttl)
		if _, err := tx.Exec(stream.Context()); err != nil {
			return err
		}

		event, err := json.Marshal(em)
		if err != nil {
			return err
		}

		// Generate a response with event data
		resp := &strelka.ScanResponse{
			Id:    req.Id,
			Event: string(event),
		}

		// Send gRPC response back to client
		span.AddEvent("Send gRPC response back to client")
		s.responses <- resp
		if err := stream.Send(resp); err != nil {
			return err
		}
	}

	return nil
}

func main() {
	confPath := flag.String(
		"c",
		"/etc/strelka/frontend.yaml",
		"path to frontend config",
	)
	flag.Parse()

	confData, err := os.ReadFile(*confPath)
	if err != nil {
		log.Fatalf("failed to read config file %s: %v", *confPath, err)
	}

	var conf structs.Frontend
	err = yaml.Unmarshal(confData, &conf)
	if err != nil {
		log.Fatalf("failed to load config data: %v", err)
	}

	listen, err := net.Listen("tcp", conf.Server)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	responses := make(chan *strelka.ScanResponse, 100)
	defer close(responses)
	if conf.Response.Log != "" {
		go func() {
			rpc.LogResponses(responses, conf.Response.Log)
		}()
		log.Printf("responses will be logged to %v", conf.Response.Log)
	} else if conf.Response.Report != 0 {
		go func() {
			rpc.ReportResponses(responses, conf.Response.Report)
		}()
		log.Printf("responses will be reported every %v", conf.Response.Report)
	} else {
		go func() {
			rpc.DiscardResponses(responses)
		}()
		log.Println("responses will be discarded")
	}

	cd := redis.NewClient(&redis.Options{
		Addr:        conf.Coordinator.Addr,
		DB:          conf.Coordinator.DB,
		PoolSize:    conf.Coordinator.Pool,
		ReadTimeout: conf.Coordinator.Read,
	})
	if err := cd.Ping(cd.Context()).Err(); err != nil {
		log.Fatalf("failed to connect to coordinator: %v", err)
	}

	gk := redis.NewClient(&redis.Options{
		Addr:        conf.Gatekeeper.Addr,
		DB:          conf.Gatekeeper.DB,
		PoolSize:    conf.Gatekeeper.Pool,
		ReadTimeout: conf.Gatekeeper.Read,
	})
	if err := gk.Ping(gk.Context()).Err(); err != nil {
		log.Fatalf("failed to connect to gatekeeper: %v", err)
	}

	s := grpc.NewServer()
	opts := &server{
		coordinator: coord{
			cli: cd,
		},
		gatekeeper: gate{
			cli: gk,
			ttl: conf.Gatekeeper.TTL,
		},
		responses: responses,
	}

	strelka.RegisterFrontendServer(s, opts)
	grpc_health_v1.RegisterHealthServer(s, opts)
	s.Serve(listen)
}
