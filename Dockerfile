# Build stage: the service is pure Go (modernc.org/sqlite needs no cgo), so the
# binary is static and the runtime image needs nothing but the binary itself.
FROM golang:1.24-alpine AS build

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE
ENV MODULE=github.com/thinkovation/service_example

WORKDIR /src

# Dependencies first: this layer is cached until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
	-ldflags "-s -w \
		-X ${MODULE}/internal/buildinfo.Version=${VERSION} \
		-X ${MODULE}/internal/buildinfo.Commit=${COMMIT} \
		-X ${MODULE}/internal/buildinfo.BuildDate=${BUILD_DATE}" \
	-o /out/customersvc ./cmd/customersvc

# Runtime stage: distroless, non-root, no shell.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/customersvc /usr/local/bin/customersvc

# Where the SQLite file lands if the image is run on the SQLite driver. Mount a
# volume here for that; with STORE_DRIVER=postgres nothing is written at all.
WORKDIR /var/lib/customersvc
ENV SQLITE_DSN=file:/var/lib/customersvc/customers.db \
	HTTP_ADDR=:8080

EXPOSE 8080

# There is no shell in the image, so the binary probes itself.
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
	CMD ["/usr/local/bin/customersvc", "healthcheck"]

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/customersvc"]
