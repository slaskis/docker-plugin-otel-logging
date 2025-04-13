build:
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

	echo "Creating the Docker plugin..."
	docker plugin rm -f otel-logger || true
	docker plugin create otel-logger .
	docker plugin enable otel-logger

	echo "Plugin created and enabled successfully!"
