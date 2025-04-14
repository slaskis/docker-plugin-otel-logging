build:
	@echo "Building rootfs for the Docker plugin..."
	# Build the plugin binary and Docker image
	docker build -t otel-logger-plugin .

	# Create a temporary container
	docker create --name tmp-otel-plugin otel-logger-plugin

	# Clean up any previous rootfs
	rm -rf rootfs

	# Extract the rootfs
	mkdir -p rootfs
	docker export tmp-otel-plugin | tar -x -C rootfs

	# Clean up the temporary container
	docker rm -f tmp-otel-plugin

	@echo "Creating the Docker plugin..."
	docker plugin rm -f slaskis/otel-logging || true
	docker plugin create slaskis/otel-logging .
	docker plugin enable slaskis/otel-logging

push:
	docker plugin push slaskis/otel-logging
