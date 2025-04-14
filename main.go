package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/fifo"
	"github.com/docker/docker/api/types/plugins/logdriver"
	"github.com/docker/docker/daemon/logger"
	protoio "github.com/gogo/protobuf/io"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
)

const (
	pluginName = "otel-logger"
)

type driver struct {
	mu   sync.Mutex
	logs map[string]*logPair
	idx  map[string]*logPair
}

type logPair struct {
	l      *otelLogger
	stream io.ReadCloser
	info   logger.Info
}

type otelLogger struct {
	logWriter   otellog.Logger
	ctx         context.Context
	cancel      context.CancelFunc
	info        logger.Info
	endpoint    string
	serviceName string
	insecure    bool
	timeout     time.Duration
}

func newDriver() *driver {
	return &driver{
		logs: make(map[string]*logPair),
		idx:  make(map[string]*logPair),
	}
}

func (d *driver) StartLogging(file string, logCtx logger.Info) error {
	d.mu.Lock()
	if _, exists := d.logs[file]; exists {
		d.mu.Unlock()
		return fmt.Errorf("logger for %q already exists", file)
	}
	d.mu.Unlock()

	log.Printf("StartLogging called for container %s, file: %s", logCtx.ContainerID, file)

	// Create a separate context for the FIFO that won't be canceled when logging stops
	fifoCtx := context.Background()

	// Create OpenTelemetry logger
	ctx, cancel := context.WithCancel(context.Background())

	config := struct {
		endpoint    string
		serviceName string
		insecure    bool
		timeout     time.Duration
	}{
		endpoint:    "localhost:4317",
		serviceName: "docker",
		insecure:    false,
		timeout:     30 * time.Second,
	}

	// Parse options
	if endpoint, exists := logCtx.Config["otel-endpoint"]; exists {
		config.endpoint = endpoint
	}
	if serviceName, exists := logCtx.Config["service-name"]; exists {
		config.serviceName = serviceName
	}
	if insecure, exists := logCtx.Config["insecure"]; exists && insecure == "true" {
		config.insecure = true
	}
	if timeout, exists := logCtx.Config["timeout"]; exists {
		if d, err := time.ParseDuration(timeout); err == nil {
			config.timeout = d
		}
	}

	// Create OTel exporter
	var opts []otlploghttp.Option
	opts = append(opts, otlploghttp.WithEndpoint(config.endpoint))
	opts = append(opts, otlploghttp.WithTimeout(config.timeout))
	if config.insecure {
		opts = append(opts, otlploghttp.WithInsecure())
	}

	logExporter, err := otlploghttp.New(ctx, opts...)
	if err != nil {
		cancel()
		return fmt.Errorf("failed to create OTel log exporter: %v", err)
	}
	log.Printf("Created OTLP exporter with endpoint: %s (insecure: %v)", config.endpoint, config.insecure)

	// Create resource with container metadata
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(config.serviceName),
			attribute.String("container.id", logCtx.ContainerID),
			attribute.String("container.name", logCtx.ContainerName),
			attribute.String("container.image", logCtx.ContainerImageName),
			attribute.String("container.image.id", logCtx.ContainerImageID),
		),
	)
	if err != nil {
		cancel()
		return fmt.Errorf("failed to create resource: %v", err)
	}

	// Create logger provider
	loggerProvider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(
			sdklog.NewBatchProcessor(logExporter),
		),
	)

	// Get a logger from the provider
	logWriter := loggerProvider.Logger(
		"docker-logs",
		otellog.WithInstrumentationVersion("0.1.0"),
	)

	ol := &otelLogger{
		logWriter:   logWriter,
		ctx:         ctx,
		cancel:      cancel,
		info:        logCtx,
		endpoint:    config.endpoint,
		serviceName: config.serviceName,
		insecure:    config.insecure,
		timeout:     config.timeout,
	}

	log.Printf("Opening FIFO at: %s", file)
	// Use fifoCtx for the FIFO, not the cancelable context
	f, err := fifo.OpenFifo(fifoCtx, file, syscall.O_RDONLY, 0700)
	if err != nil {
		cancel()
		return errors.Wrapf(err, "error opening logger fifo: %q", file)
	}

	d.mu.Lock()
	lf := &logPair{ol, f, logCtx}
	d.logs[file] = lf
	d.idx[logCtx.ContainerID] = lf
	d.mu.Unlock()

	go consumeLog(lf)
	return nil
}

func (d *driver) StopLogging(file string) error {
	log.Printf("StopLogging called for file: %s", file)
	d.mu.Lock()
	lf, ok := d.logs[file]
	if ok {
		lf.stream.Close()
		lf.l.cancel() // Cancel the context
		delete(d.logs, file)
		delete(d.idx, lf.info.ContainerID)
	}
	d.mu.Unlock()
	return nil
}

func consumeLog(lf *logPair) {
	log.Printf("Starting to consume logs for container: %s", lf.info.ContainerID)

	// Use a more robust approach to reading the FIFO
	dec := protoio.NewUint32DelimitedReader(lf.stream, binary.BigEndian, 1e6)
	defer dec.Close()

	var buf logdriver.LogEntry
	var err error

	for {
		err = dec.ReadMsg(&buf)
		if err != nil {
			if err == io.EOF {
				log.Printf("EOF reached for container %s", lf.info.ContainerID)
				break
			}

			if strings.Contains(err.Error(), "file already closed") {
				log.Printf("FIFO closed for container %s, exiting reader", lf.info.ContainerID)
				break
			}

			log.Printf("Error reading log message for container %s: %v - will retry",
				lf.info.ContainerID, err)

			// Give a short delay before retrying
			time.Sleep(100 * time.Millisecond)

			// Don't try to recreate the reader - just continue and let it error again if needed
			continue
		}

		// Successfully read a message, process it
		timestamp := time.Unix(0, buf.TimeNano)

		record := otellog.Record{}
		record.SetTimestamp(timestamp)
		record.SetSeverity(otellog.SeverityInfo)
		record.SetBody(otellog.StringValue(string(buf.Line)))
		record.AddAttributes(
			otellog.String("container.id", lf.info.ContainerID),
			otellog.String("container.name", lf.info.ContainerName),
			otellog.String("stream", buf.Source),
		)

		// If line is JSON, try to parse it for additional attributes
		var jsonMsg map[string]any
		if err := json.Unmarshal(buf.Line, &jsonMsg); err == nil {
			for k, v := range jsonMsg {
				if str, ok := v.(string); ok {
					record.AddAttributes(otellog.String(k, str))
				}
			}
		}

		// Emit log with a background context that won't be canceled
		ctx, cancel := context.WithTimeout(context.Background(), lf.l.timeout)

		log.Printf("Emitting log record to OTLP for container %s", lf.info.ContainerID)
		lf.l.logWriter.Emit(ctx, record)
		cancel()

		buf.Reset()
	}

	// Clean up
	log.Printf("Log consumer for container %s is exiting", lf.info.ContainerID)
	lf.stream.Close() // Ensure stream is closed
}

func (d *driver) Name() string {
	return pluginName
}

// Implementing minimal ReadLogs interface
func (d *driver) ReadLogs(info logger.Info, config logger.ReadConfig) (io.ReadCloser, error) {
	return nil, fmt.Errorf("reading logs is not supported")
}

func main() {
	// Setup logging
	logFile, err := os.OpenFile("/var/log/otel-plugin.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		log.SetOutput(logFile)
	}

	log.Println("OpenTelemetry logging plugin starting up")

	// Create and serve the plugin
	d := newDriver()
	h := &pluginHandler{driver: d}

	// Setup HTTP handlers for Docker plugin API
	http.HandleFunc("/Plugin.Activate", h.activate)
	http.HandleFunc("/LogDriver.StartLogging", h.startLogging)
	http.HandleFunc("/LogDriver.StopLogging", h.stopLogging)
	http.HandleFunc("/LogDriver.Capabilities", h.capabilities)

	// Create socket path
	socketPath := "/run/docker/plugins/otel.sock"

	// Clean up any existing socket
	if _, err := os.Stat(socketPath); err == nil {
		log.Printf("Removing existing socket: %s", socketPath)
		os.Remove(socketPath)
	}

	log.Printf("Creating socket at: %s", socketPath)

	// Create socket directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		log.Fatalf("Error creating socket directory: %v", err)
	}

	// Listen on Unix socket
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatalf("Error creating socket: %v", err)
	}

	log.Printf("Socket created successfully at %s", socketPath)

	// Create a marker file that Docker looks for to identify plugins
	marker := filepath.Join(filepath.Dir(socketPath), pluginName+".name")
	if err := os.WriteFile(marker, []byte(pluginName), 0644); err != nil {
		log.Printf("Error creating plugin marker file: %v", err)
	}

	// Serve HTTP requests on this socket
	log.Println("Starting HTTP server on Unix socket")
	if err := http.Serve(l, nil); err != nil {
		log.Fatalf("Error serving HTTP on socket: %v", err)
	}
}

type pluginHandler struct {
	driver *driver
}

type startLoggingRequest struct {
	File string
	Info logger.Info
}

type stopLoggingRequest struct {
	File string
}

func (h *pluginHandler) activate(w http.ResponseWriter, r *http.Request) {
	log.Println("Plugin.Activate called")
	json.NewEncoder(w).Encode(map[string]any{
		"Implements": []string{"LogDriver"},
	})
}

func (h *pluginHandler) capabilities(w http.ResponseWriter, r *http.Request) {
	log.Println("LogDriver.Capabilities called")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"Cap": map[string]bool{
			"ReadLogs": false,
		},
	})
}

func (h *pluginHandler) startLogging(w http.ResponseWriter, r *http.Request) {
	log.Println("LogDriver.StartLogging called")

	var req startLoggingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("Error decoding StartLogging request: %v", err)
		return
	}

	log.Printf("StartLogging request: file=%s, container=%s",
		req.File, req.Info.ContainerID)

	if err := h.driver.StartLogging(req.File, req.Info); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("Error starting logging: %v", err)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{})
}

func (h *pluginHandler) stopLogging(w http.ResponseWriter, r *http.Request) {
	log.Println("LogDriver.StopLogging called")

	var req stopLoggingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("Error decoding StopLogging request: %v", err)
		return
	}

	log.Printf("StopLogging request: file=%s", req.File)

	if err := h.driver.StopLogging(req.File); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("Error stopping logging: %v", err)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{})
}
