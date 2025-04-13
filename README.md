# Docker OpenTelemetry Logging Plugin

This Docker logging plugin captures container logs and forwards them to an OpenTelemetry collector.

## Installation

Build and install the plugin:

```bash
docker plugin create otel-logger .
docker plugin enable otel-logger
