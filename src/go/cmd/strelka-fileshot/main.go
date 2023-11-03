package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/natefinch/lumberjack"
	"github.com/target/strelka/src/go/api/strelka"
	"github.com/target/strelka/src/go/pkg/rpc"
	"github.com/target/strelka/src/go/pkg/structs"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/gabriel-vasile/mimetype"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v2"
)

type matchRich struct {
	filePath string
	fileSize int64
	modTime  time.Time
	pattern string
}

func main() {
	// Declare flags
	configPath := flag.String("c", "/etc/strelka/fileshot.yaml", "Path to fileshot configuration file.")
	hashPath := flag.String("e", "", "Path to hash exclusions list.")
	verbose := flag.Bool("v", false, "Enables additional error logging.")
	cpuProf := flag.Bool("cpu", false, "Enables cpu profiling.")
	heapProf := flag.Bool("heap", false, "Enables heap profiling.")
	dryRun := flag.Bool("dry", false, "Runs fileshot in dry run mode (no requests).")

	// Parse flags
	flag.Parse()

	// Check if CPU profiling is enabled
	if *cpuProf {
		// Create file for CPU profiling data
		cpu, err := os.Create("./cpu.pprof")
		if err != nil {
			log.Fatalf("failed to create cpu.pprof file: %v", err)
		}

		// Start CPU profiling
		pprof.StartCPUProfile(cpu)
		defer pprof.StopCPUProfile()
	}

	// Read configuration file
	confData, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("failed to read config file %s: %v", *configPath, err)
	}

	// Try to recover absolute config file path
	absConfigPath, _ := filepath.Abs(*configPath)
	if absConfigPath == "" {
		absConfigPath = *configPath
	}

	// Create a new config hash
	configHasher := md5.New()
	configHasher.Write([]byte(confData))

	// Unmarshal configuration data into struct
	var conf structs.FileShot
	err = yaml.Unmarshal(confData, &conf)
	if err != nil {
		log.Fatalf("failed to load config data: %v", err)
	}

	if conf.Log.Verbose {
		*verbose = conf.Log.Verbose
	}

	// Configure rotating log file sink which tees to stdout
	if conf.Log.Path != "" {

		maxSize := 1
		maxBackups := 0
		maxAge := 7

		if conf.Log.MaxSize > 0 {
			maxSize = conf.Log.MaxSize
		}
		if conf.Log.MaxBackups > 0 {
			maxBackups = conf.Log.MaxBackups
		}
		if conf.Log.MaxAge > 0 {
			maxAge = conf.Log.MaxAge
		}

		mw := io.MultiWriter(os.Stdout, &lumberjack.Logger{
			Filename:   conf.Log.Path,
			MaxSize:    maxSize,
			MaxBackups: maxBackups,
			MaxAge:     maxAge,
		})

		log.SetOutput(mw)

	}

	if conf.Version != "" {
		log.Printf("Loaded config file v%s %s %s", conf.Version, fmt.Sprintf("%x", configHasher.Sum(nil)), absConfigPath)
	} else {
		log.Printf("Loaded config file %s %s", fmt.Sprintf("%x", configHasher.Sum(nil)), absConfigPath)
	}

	// Create a slice to hold the lines of the file
	hashes := make([]string, 0)

	// Check if hash exclusion path is set
	// Load exclusion hashes
	if *hashPath != "" || conf.Files.ExcludeHashes.Path != "" || len(conf.Files.ExcludeHashes.List) > 0 {

		if *hashPath == "" {
			*hashPath = conf.Files.ExcludeHashes.Path
		}

		if *hashPath != "" {
			hashData, err := os.ReadFile(*hashPath)
			if err != nil {
				log.Fatalf("failed to read hash exclusion file %s: %v", *hashPath, err)
			}

			// Create a new Scanner to read the hash data
			hashScanner := bufio.NewScanner(strings.NewReader(string(hashData)))

			// Iterate through the lines of the file
			for hashScanner.Scan() {
				// Append the current line to the slice
				// Convert the string to an int64
				i := hashScanner.Text()
				hashes = append(hashes, i)
			}
		}

		hashes = append(hashes, conf.Files.ExcludeHashes.List...)

	}

	if *dryRun {

		getFilePaths(conf, verbose, hashes)

	} else {

		// Dial server using configuration data
		serv := conf.Conn.Server
		auth := rpc.SetAuth(conf.Conn.Cert)
		ctx, cancel := context.WithTimeout(context.Background(), conf.Conn.Timeout.Dial)
		defer cancel()
		conn, err := grpc.DialContext(ctx, serv, auth, grpc.WithBlock())
		if err != nil {
			log.Fatalf("failed to connect to %s: %v", serv, err)
		}
		defer conn.Close()

		// Create WaitGroup for managing goroutines
		var wgRequest sync.WaitGroup
		var wgResponse sync.WaitGroup

		// Create client for communicating with server
		frontend := strelka.NewFrontendClient(conn)

		// Create channel for limiting concurrency
		sem := make(chan int, conf.Throughput.Concurrency)
		defer close(sem)

		// Create channel for receiving responses from server
		responses := make(chan *strelka.ScanResponse, 100)
		defer close(responses)

		// Increment WaitGroup counter
		wgResponse.Add(1)

		// Create goroutine for handling responses
		go func() {
			if conf.Response.Log != "" {
				rpc.LogResponses(responses, conf.Response.Log)
			} else if conf.Response.Report != 0 {
				rpc.ReportResponses(responses, conf.Response.Report)
			} else {
				rpc.DiscardResponses(responses)
			}
			wgResponse.Done()
		}()

		// Print log message based on response handling configuration
		if conf.Response.Log != "" {
			log.Printf("responses will be logged to %v", conf.Response.Log)
		} else if conf.Response.Report != 0 {
			log.Printf("responses will be reported every %v", conf.Response.Report)
		} else {
			log.Println("responses will be discarded")
		}

		// Use default client name if not specified in configuration
		client := "go-fileshot"
		if conf.Client != "" {
			client = conf.Client
		}

		// Get hostname
		hostname, err := os.Hostname()
		if err != nil {
			log.Fatalf("failed to retrieve hostname: %v", err)
		}

		// Create request object
		request := &strelka.Request{
			Client:     client,
			Source:     hostname,
			Gatekeeper: conf.Files.Gatekeeper,
		}

		for _, path := range getFilePaths(conf, verbose, hashes) {
			// Create the ScanFileRequest struct with the provided attributes.
			req := structs.ScanFileRequest{
				Request: request,
				Attributes: &strelka.Attributes{
					Filename: path,
				},
				Chunk:  conf.Throughput.Chunk,
				Delay:  conf.Throughput.Delay,
				Delete: conf.Files.Delete,
			}

			// Send the ScanFileRequest to the RPC server using a goroutine.
			sem <- 1
			wgRequest.Add(1)
			go func() {
				rpc.ScanFile(
					frontend,
					conf.Conn.Timeout.File,
					req,
					responses,
				)

				// Notify the wgRequest wait group that the goroutine has finished.
				wgRequest.Done()

				// Release the semaphore to indicate that the goroutine has finished.
				<-sem
			}()
		}

		wgRequest.Wait()
		responses <- nil
		wgResponse.Wait()

	}

	// Check if the heapProf flag is set and write a heap profile if it is.
	if *heapProf {
		// Create the heap.pprof file.
		heap, err := os.Create("./heap.pprof")
		if err != nil {
			log.Fatalf("failed to create heap.pprof file: %v", err)
		}

		// Write the heap profile to the file.
		pprof.WriteHeapProfile(heap)

		// Close the file to release associated resources.
		heap.Close()
	}
}

// Checks if the size of a file is within a given range and returns
// true if it is, or false otherwise.
func checkFileSize(fileSize int64, minSize int64, maxSize int64, verbose bool) (bool, int64) {
	// Check if the file size is within the specified range
	if fileSize >= minSize && fileSize <= maxSize {
		return true, fileSize
	}

	return false, fileSize
}

// Checks the MIME type of a file against a list of MIME types and returns
// true if a match is found, or false otherwise.
func checkFileMimetype(file *os.File, positivemimetypes []string, negativemimetypes []string, verbose bool) (bool, string) {
	
	// Make sure the file pointer is at 0
	file.Seek(0, 0)
	
	// Read the first 512 bytes of the file
	buffer := make([]byte, 512)
	_, err := file.Read(buffer)
	if err != nil {
		log.Printf("failed to read file %s: %v", file.Name(), err)
		return false, "[No Type]"
	}

	// Determine the MIME type of the file based on its content
	mimeType := mimetype.Detect(buffer)

	if mimeType.String() == "" {
		// If the MIME type could not be determined, log an error and continue to the next file.
		log.Printf("failed to determine MIME type for file %s", file.Name())
		return false, "[No Type]"
	}

	// Normalize MIME type to remove subtypes, e.g. text/plain; charset=utf-16le
	mimeTypeNormal := strings.Split(mimeType.String(), ";")[0]

	if len(negativemimetypes) > 0 {
		// Iterate through the list of denied MIME types
		for _, v := range negativemimetypes {
			// Check if the current MIME type matches a known MIME type
			if strings.Contains(mimeTypeNormal, v) {
				if verbose {
					log.Printf("[IGNORING] File mimetype (%s) is not within negative configured list of mimetypes: %s.", mimeTypeNormal, file.Name())
				}
				return false, mimeTypeNormal
			}
		}

		return true, mimeTypeNormal
	} else if len(positivemimetypes) > 0 {
		// Iterate through the list of allowed MIME types
		for _, v := range positivemimetypes {
			// Check if the current MIME type matches a known MIME type
			if strings.Contains(mimeTypeNormal, v) {
				if verbose {
					log.Printf("Matched positive file mimetype (%s): %s.", mimeTypeNormal, file.Name())
				}
				return true, mimeTypeNormal
			}
		}

		if verbose {
			log.Printf("[IGNORING] File mimetype (%s) is not within positive configured list of mimetypes: %s.", mimeTypeNormal, file.Name())
		}
	
		return false, mimeTypeNormal
	}

	return false, mimeTypeNormal
}

// checkFileHash checks the hash of a file against a list of hashes and returns
// true if a match is found, or false otherwise.
func checkFileHash(file *os.File, hashlist []string, verbose bool) (bool, string) {

	// Make sure the file pointer is at 0
	file.Seek(0, 0)

	// Create a new MD5 hash
	hash := md5.New()

	// Read the contents of the file into the hash
	if _, err := io.Copy(hash, file); err != nil {
		log.Printf("File could not be hashed: %s %s", file.Name(), err)
		return false, "[No Hash]"
	}

	// Iterate through the list of exclusion hashes
	// Return true if file hash matches an exclusion hash.
	for _, s := range hashlist {
		if s == fmt.Sprintf("%x", hash.Sum(nil)) {
			if verbose {
				log.Printf("[IGNORING] File hash (%s) was found in hash exclusion list.", fmt.Sprintf("%x", hash.Sum(nil)))
			}
			return false, fmt.Sprintf("%x", hash.Sum(nil))
		}
	}
	return true, fmt.Sprintf("%x", hash.Sum(nil))
}

// checkRecentlyModified checks to see if the file was modified outside the specified timeframe.
// true if modified within the specified timeframe, false otherwise
func checkRecentlyModified(modTime time.Time, modified int, verbose bool) (bool, time.Time) {
	now := time.Now() // Get the current time

	if now.Sub(modTime) <= time.Duration(modified)*time.Hour {
		return true, modTime
	}

	return false, modTime
}

// wildCardToRegexp converts a wildcard pattern to a regular expression pattern.
func wildCardToRegexp(pattern string) string {
	var result strings.Builder
	for i, literal := range strings.Split(pattern, "*") {

		// Replace * with .*
		if i > 0 {
			result.WriteString(".*")
		}

		// Quote any regular expression meta characters in the
		// literal text.
		result.WriteString(regexp.QuoteMeta(literal))
	}
	return result.String()
}

func sortMatches(matches []matchRich, field string, order string) []matchRich {

	if field == "time" {
		if order == "asc" { 
			sort.Slice(matches, func(i, j int) bool {
				return matches[i].modTime.Before(matches[j].modTime)
			})
		} else if order == "desc" {
			sort.Slice(matches, func(i, j int) bool {
				return matches[i].modTime.After(matches[j].modTime) 
			})
		}
	} else if field == "path" {
		if order == "asc" { 
			sort.Slice(matches, func(i, j int) bool {
				return matches[i].filePath < matches[j].filePath
			})
		} else if order == "desc" {
			sort.Slice(matches, func(i, j int) bool {
				return matches[i].filePath > matches[j].filePath
			})
		}
	} else if field == "size" {
		if order == "asc" { 
			sort.Slice(matches, func(i, j int) bool {
				return matches[i].fileSize < matches[j].fileSize
			})
		} else if order == "desc" {
			sort.Slice(matches, func(i, j int) bool {
				return matches[i].fileSize > matches[j].fileSize
			})
		}
	}

	return matches

}

func getFilePaths(conf structs.FileShot, verbose *bool, hashes []string) []string {

	paths := make([]string, 0)
	var matchRichFiles []matchRich
	patternLimitReached := false
	capacityLimitReached := false

	// Set Total Limiter for Max Files to be consumed by host
	totalCount := 0
	// Set Total Limiter for Max Capacity to be consumed per run
	var capacityCount int64

	// var patternCount map[string]int
	patternCount := make(map[string]int)

	for _, pattern := range conf.Files.Patterns.Positive {
		patternCount[pattern] = 0
	}

	// Loop through each pattern in the list of file patterns
	for _, p := range conf.Files.Patterns.Positive {

		if *verbose {
			log.Printf("Collecting files from: %s", p)
		}

		// Expand the pattern to a list of matching file paths
		match, err := doublestar.FilepathGlob(p)
		if err != nil {
			log.Printf("failed to glob pattern %s: %v", p, err)
			continue
		}

		// Loop through the provided slice of file names
	OUTERMATCH:
		for _, filePath := range match {

			// Get the file info and handle any errors
			fileInfo, err := os.Stat(filePath)
			if err != nil {
				if *verbose {
					log.Printf("failed to stat file %s: %v", filePath, err)
				}
				continue
			}

			// Check if the file is a regular file (not a directory or symlink, etc.)
			if fileInfo.Mode()&os.ModeType != 0 {
				continue
			}

			for _, negativePattern := range conf.Files.Patterns.Negative {
				result, _ := regexp.MatchString(wildCardToRegexp(negativePattern), filePath)

				if result {
					if *verbose {
						log.Printf("[IGNORING] Negative pattern matched: %s %s", negativePattern, filePath)
					}
					continue OUTERMATCH
				}
			}

			matchRichFiles = append(matchRichFiles, matchRich{
				filePath: filePath,
				fileSize: fileInfo.Size(),
				modTime:  fileInfo.ModTime(),
				pattern: p,
			})

		}
	}

	// Sort the file path matches according to the configuration
	if conf.Files.Sort.Time != "" {
		matchRichFiles = sortMatches(matchRichFiles, "time", conf.Files.Sort.Time)
	} else if conf.Files.Sort.Path != "" {
		matchRichFiles = sortMatches(matchRichFiles, "path", conf.Files.Sort.Path)
	} else if conf.Files.Sort.Size != "" {
		matchRichFiles = sortMatches(matchRichFiles, "size", conf.Files.Sort.Size)
	} else {
		matchRichFiles = sortMatches(matchRichFiles, "time", "desc")
	}

	// Iterate over the list of files that match the provided pattern.
	// Order of operation for gate processing:
	//      1) If the total number of files collected crosses the specified limit, end collection
	//      2) If the total number of files collected for a specific pattern crosses a specified limit, skip
	//      3) If the modification date for a file is older than specified, skip
	//		4) If the file is outside the specified min/max file size, skip
	//      5) If the file would exceed the specified capacity limit, skip
	//		6) If the file mimetype is not included or excluded by type specification, skip
	//	 	7) If the file hash matches the specified exclusion list, skip
	//      8) File staged for sending
	for _, f := range matchRichFiles {

		var fileSize int64 = 0
		var fileHash string = "[No Hash]"
		var fileType string = "[No Type]"
		var modTime time.Time
		var checkModified bool
		var checkSize bool
		var checkHash bool
		var checkType bool

		// If total files collected exceeds amount allowed per collection, finish.
		if conf.Files.LimitTotal > 0 && totalCount >= conf.Files.LimitTotal {
			log.Printf("[LIMIT REACHED] Total file collection limit of %d reached.", conf.Files.LimitTotal)
			break
		}

		// If current path exceeds amount allowed per collection path, move onto next path.
		// log.Println(f.pattern, patternCount[f.pattern])
		if conf.Files.LimitPattern > 0 && patternCount[f.pattern] >= conf.Files.LimitPattern {
			// Only show the pattern limit warning once per pattern
			if !patternLimitReached {
				log.Printf("[LIMIT REACHED] Total pattern collection limit of %d reached for %s.", conf.Files.LimitPattern, f.pattern)
				patternLimitReached = true
			}
			continue
		}

		// GATE CHECK
		// Check file modification time
		// If recently modified is set, run this, otherwise place match into new var matches
		if conf.Files.Modified > 0 {
			checkModified, modTime = checkRecentlyModified(f.modTime, conf.Files.Modified, *verbose)
			if !checkModified {
				if *verbose {
					log.Printf("[IGNORING] Last modified time: %s older than configured timeframe (%d hours): %s.", modTime, conf.Files.Modified, f.filePath)
				}
				continue
			}
		}

		// GATE CHECK
		// Check file size
		// If file size not in range, skip to next file.
		checkSize, fileSize = checkFileSize(f.fileSize, int64(conf.Files.Minsize), int64(conf.Files.Maxsize), *verbose)
		if !(conf.Files.Minsize < 0) && conf.Files.Maxsize > 0 {
			if !checkSize {
				if *verbose {
					log.Printf("[IGNORING] File size (%d) is not within configured Minsize (%d) and Maxsize (%d): %s", fileSize, int64(conf.Files.Minsize), int64(conf.Files.Maxsize), f.filePath)
				}
				continue
			}
		}

		// If current path exceeds amount of capacity allowed, move onto next path.
		if conf.Files.LimitCapacity > 0 && (capacityCount+f.fileSize) > conf.Files.LimitCapacity {
			if !capacityLimitReached {
				capacityLimitReached = true
			}
			if *verbose {
				log.Printf("[LIMIT REACHED] File would exceed total capacity limit of %d on %s", conf.Files.LimitCapacity, f.filePath)
			}
			continue
		}

		// Files need to be opened if any of these config conditions are true
		if len(conf.Files.Mimetypes.Positive) > 0 || len(conf.Files.Mimetypes.Negative) > 0 || len(hashes) > 0 {

			// Open the file
			file, err := os.Open(f.filePath)
			if err != nil {
				if *verbose {
					log.Printf("failed to open file %s: %v", f.filePath, err)
				}
				continue
			}
			defer file.Close()

			// GATE CHECK
			// Check file mimetypes
			// If mimetype doesn't match the filters, skip to next file.
			if len(conf.Files.Mimetypes.Positive) > 0 || len(conf.Files.Mimetypes.Negative) > 0 {
				checkType, fileType = checkFileMimetype(file, conf.Files.Mimetypes.Positive, conf.Files.Mimetypes.Negative, *verbose)
				if !checkType {
					continue
				}
			}

			// GATE CHECK
			// Check hash exclusions
			// If an exclusion is found, skip to next file.
			if len(hashes) > 0 {
				checkHash, fileHash = checkFileHash(file, hashes, *verbose)
				if !checkHash {
					continue
				}
			}
		}

		// Iterate Limiter Counts
		totalCount += 1
		patternCount[f.pattern] += 1
		capacityCount += fileSize

		paths = append(paths, f.filePath)

		log.Printf("File staged for submission: %s %d %s %s %s", f.filePath, fileSize, fileType, fileHash, modTime)
	}

	if capacityLimitReached {
		log.Printf("[LIMIT REACHED] File collection was limited by total capacity limit of %d. Use verbose logging mode to learn more.", conf.Files.LimitCapacity)
	}

	log.Printf("Collected %d total files.", totalCount)

	return paths
}
