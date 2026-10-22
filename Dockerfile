# Build stage: the service is pure Go (modernc.org/sqlite needs no cgo), so the
# binary is static and the runtime image needs nothing but the binary itself.
FROM golang:1.24-alpine AS build

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE
ENV MODULE=github.com/thinkovation/service_example

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
	-ldflags "-s -w \
		-X ${MODULE}/internal/buildinfo.Version=${VERSION} \
		-X ${MODULE}/internal/buildinfo.Commit=${COMMIT} \
		-X ${MODULE}/internal/buildinfo.BuildDate=${BUILD_DATE}" \
	-o /out/customersvc ./cmd/customersvc

# Runtime stage.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/customersvc /usr/local/bin/customersvc

# The SQLite default writes here; mount a volume when running without Postgres.
WORKDIR /var/lib/customersvc
ENV SQLITE_DSN=file:/var/lib/customersvc/customers.db

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/customersvc"]
