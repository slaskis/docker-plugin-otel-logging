package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/docker/docker/daemon/logger"
	"github.com/docker/go-plugins-helpers/sdk"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
)

const (
	name = "otel"
)

// Configuration options for the OTel log driver
type otelLogOpt struct {
	endpoint    string
	serviceName string
	insecure    bool
	timeout     time.Duration
}

type otelLogger struct {
	containerID   string
	containerName string
	logWriter     log.Logger
	info          logger.Info
	config        otelLogOpt
	ctx           context.Context
	cancel        context.CancelFunc
}

// newOTelLogger creates a new logger for Docker that sends logs to OpenTelemetry
func newOTelLogger(info logger.Info) (logger.Logger, error) {
	ctx, cancel := context.WithCancel(context.Background())

	config := otelLogOpt{
		endpoint:    "localhost:4317",
		serviceName: "docker",
		insecure:    false,
		timeout:     30 * time.Second,
	}

	// Parse options
	if endpoint, exists := info.Config["otel-endpoint"]; exists {
		config.endpoint = endpoint
	}
	if serviceName, exists := info.Config["service-name"]; exists {
		config.serviceName = serviceName
	}
	if insecure, exists := info.Config["insecure"]; exists && insecure == "true" {
		config.insecure = true
	}
	if timeout, exists := info.Config["timeout"]; exists {
		if d, err := time.ParseDuration(timeout); err == nil {
			config.timeout = d
		}
	}

	// Create OTel exporter
	logExporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(config.endpoint),
		otlploggrpc.WithTimeout(config.timeout),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create OTel log exporter: %v", err)
	}

	// Create resource with container metadata
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(config.serviceName),
			attribute.String("container.id", info.ContainerID),
			attribute.String("container.name", info.ContainerName),
			attribute.String("container.image", info.ContainerImageName),
			attribute.String("container.image.id", info.ContainerImageID),
		),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create resource: %v", err)
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
		log.WithInstrumentationVersion("0.1.0"),
	)

	return &otelLogger{
		containerID:   info.ContainerID,
		containerName: info.ContainerName,
		logWriter:     logWriter,
		info:          info,
		config:        config,
		ctx:           ctx,
		cancel:        cancel,
	}, nil
}

// Log implements the logger.Logger interface
func (l *otelLogger) Log(msg *logger.Message) error {
	// Create log record
	record := log.Record{}
	record.SetTimestamp(msg.Timestamp)
	record.SetSeverity(log.SeverityInfo)
	record.SetBody(log.StringValue(string(msg.Line)))
	record.AddAttributes(
		log.String("container.id", l.containerID),
		log.String("container.name", l.containerName),
		log.String("stream", msg.Source),
	)

	// If line is JSON, try to parse it for additional attributes
	var jsonMsg map[string]interface{}
	if err := json.Unmarshal(msg.Line, &jsonMsg); err == nil {
		for k, v := range jsonMsg {
			if str, ok := v.(string); ok {
				record.AddAttributes(log.String(k, str))
			}
		}
	}

	// Emit log
	ctx, cancel := context.WithTimeout(l.ctx, l.config.timeout)
	defer cancel()

	l.logWriter.Emit(ctx, record)
	return nil
}

// Close implements the logger.Logger interface
func (l *otelLogger) Close() error {
	l.cancel()
	return nil
}

// Name returns the name of the logging driver
func (l *otelLogger) Name() string {
	return name
}

func main() {
	h := &pluginHandler{driver: &driver{}}
	handlers := sdk.NewHandler(`{"Implements": ["LoggingDriver"]}`)

	handlers.HandleFunc("/LogDriver.StartLogging", h.startLogging)
	handlers.HandleFunc("/LogDriver.StopLogging", h.stopLogging)
	handlers.HandleFunc("/LogDriver.Capabilities", h.capabilities)

	err := handlers.ServeUnix("otel", 0)
	if err != nil {
		fmt.Printf("Error serving plugin handler: %v\n", err)
		os.Exit(1)
	}
}

type driver struct{}

func (d *driver) StartLogging(file string, info logger.Info) (logger.Logger, error) {
	return newOTelLogger(info)
}

func (d *driver) StopLogging(file string) error {
	return nil
}

func (d *driver) Name() string {
	return name
}

func (d *driver) GetReadWriter(info logger.Info) (io.ReadCloser, io.WriteCloser, error) {
	return nil, nil, fmt.Errorf("not implemented")
}

func handlePluginActivation() {
	h := &pluginHandler{driver: &driver{}}
	handlers := sdk.NewHandler(`{"Implements": ["LoggingDriver"]}`)

	handlers.HandleFunc("/LogDriver.StartLogging", h.startLogging)
	handlers.HandleFunc("/LogDriver.StopLogging", h.stopLogging)
	handlers.HandleFunc("/LogDriver.Capabilities", h.capabilities)

	err := handlers.ServeUnix("otel", 0)
	if err != nil {
		fmt.Printf("Error serving plugin handler: %v\n", err)
		os.Exit(1)
	}
}

type pluginHandler struct {
	driver  *driver
	loggers map[string]logger.Logger
}

type startLoggingRequest struct {
	File string
	Info logger.Info
}

type stopLoggingRequest struct {
	File string
}

func (h *pluginHandler) capabilities(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"Cap": map[string]bool{
			"ReadLogs": false,
		},
	})
}

func (h *pluginHandler) startLogging(w http.ResponseWriter, r *http.Request) {
	var req startLoggingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	driverLogger, err := h.driver.StartLogging(req.File, req.Info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if h.loggers == nil {
		h.loggers = make(map[string]logger.Logger)
	}
	h.loggers[req.File] = driverLogger

	json.NewEncoder(w).Encode(map[string]string{})
}

func (h *pluginHandler) stopLogging(w http.ResponseWriter, r *http.Request) {
	var req stopLoggingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if logger, exists := h.loggers[req.File]; exists {
		if err := logger.Close(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		delete(h.loggers, req.File)
	}

	if err := h.driver.StopLogging(req.File); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{})
}
