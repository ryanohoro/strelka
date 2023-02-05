package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

//    "encoding/json"

	"google.golang.org/grpc"

	"github.com/target/strelka/src/go/api/strelka"
	"github.com/target/strelka/src/go/pkg/rpc"
	"github.com/target/strelka/src/go/pkg/structs"

	"go.opentelemetry.io/otel"
	//	"go.opentelemetry.io/otel/attribute"
	//	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/jaeger"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
)

const service_name = "strelka.client"

func newExporter(w io.Writer) (trace.SpanExporter, error) {
	return stdouttrace.New(
		stdouttrace.WithWriter(w),
		// Use human-readable output.
		stdouttrace.WithPrettyPrint(),
		// Do not print timestamps for the demo.
		stdouttrace.WithoutTimestamps(),
	)
}

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

// Keys returns the keys for which this carrier has a value.
func (c *TextMapCarrier) Keys() []string {
	result := make([]string, 0, len(c.data))
	for k := range c.data {
		result = append(result, k)
	}
	return result
}

// Get returns the value associated with the passed key.
func (c *TextMapCarrier) Get(key string) string {
	return c.data[key]
}

// Set stores the key-value pair.
func (c *TextMapCarrier) Set(key, value string) {
	fmt.Println("setting")
	fmt.Println(key)
	c.data[key] = value
}

func main() {

	mainCtx := context.Background()

	exp, err := jaeger.New(jaeger.WithCollectorEndpoint(jaeger.WithEndpoint("http://127.0.0.1:14268/api/traces")))
	if err != nil {
		panic(err)
	}

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

	newCtx, span := otel.Tracer(service_name).Start(mainCtx, "request")
	defer span.End()
	_ = newCtx // TEMP

	const (
        supportedVersion  = 0
        maxVersion        = 254
        traceparentHeader = "traceparent"
        tracestateHeader  = "tracestate"
    )

	sc := span.SpanContext()
	if !sc.IsValid() {
		return
	}

	if ts := sc.TraceState().String(); ts != "" {
		fmt.Println(string(ts))
	}

	// Clear all flags other than the trace-context supported sampling bit.
	flags := sc.TraceFlags() & 0x01

	h := fmt.Sprintf("%.2x-%s-%s-%s",
		supportedVersion,
		sc.TraceID(),
		sc.SpanID(),
		flags)

	fmt.Println(h)

	frontendUrl := flag.String("s", "127.0.0.1:57314", "url for the strelka frontend server")
	connCert := flag.String("c", "", "path to connection certificate")
	logPath := flag.String("l", "strelka-oneshot.log", "path to response log file, - for stdout")
	scanFile := flag.String("f", "", "file to submit for scanning")
	scanTimeout := flag.Int("t", 60, "scanning timeout in seconds")

	flag.Parse()

	// scanFile is mandatory
	if *scanFile == "" {
		flag.Usage()
		os.Exit(1)
	}

	if os.Getenv("STRELKA_ONESHOT_FRONTENDURL") != "" {
		*frontendUrl = os.Getenv("STRELKA_ONESHOT_FRONTENDURL")
	}
	if os.Getenv("STRELKA_ONESHOT_LOGPATH") != "" {
		*logPath = os.Getenv("STRELKA_ONESHOT_LOGPATH")
	}

	serv := *frontendUrl
	auth := rpc.SetAuth(*connCert)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	conn, err := grpc.DialContext(ctx, serv, auth, grpc.WithBlock())
	if err != nil {
		log.Fatalf("failed to connect to %s: %v", serv, err)
	}
	defer conn.Close()

	var wgResponse sync.WaitGroup

	frontend := strelka.NewFrontendClient(conn)
	responses := make(chan *strelka.ScanResponse, 100)
	defer close(responses)

	wgResponse.Add(1)
	go func() {
		if *logPath == "-" {
			rpc.PrintResponses(responses)
		} else {
			rpc.LogResponses(responses, *logPath)
		}
		wgResponse.Done()
	}()

	client := "go-oneshot"

	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("failed to retrieve hostname: %v", err)
	}

	request := &strelka.Request{
		Client:     client,
		Source:     hostname,
		Gatekeeper: false,
		Tracecontext: h,
	}

	req := structs.ScanFileRequest{
		Request: request,
		Attributes: &strelka.Attributes{
			Filename: *scanFile,
		},
		Chunk:  32768,
		Delay:  time.Second * 0,
		Delete: false,
	}

	span.AddEvent("Running ScanFile")
	rpc.ScanFile(frontend, time.Second*time.Duration(*scanTimeout), req, responses)

	responses <- nil
	wgResponse.Wait()
}
