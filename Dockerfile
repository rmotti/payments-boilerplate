# syntax=docker/dockerfile:1

FROM golang:1.26.7-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/rmotti/payments-boilerplate/internal/platform/buildinfo.Version=${VERSION} -X github.com/rmotti/payments-boilerplate/internal/platform/buildinfo.Commit=${COMMIT} -X github.com/rmotti/payments-boilerplate/internal/platform/buildinfo.BuildTime=${BUILD_TIME}" \
    -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/rmotti/payments-boilerplate/internal/platform/buildinfo.Version=${VERSION} -X github.com/rmotti/payments-boilerplate/internal/platform/buildinfo.Commit=${COMMIT} -X github.com/rmotti/payments-boilerplate/internal/platform/buildinfo.BuildTime=${BUILD_TIME}" \
    -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/rmotti/payments-boilerplate/internal/platform/buildinfo.Version=${VERSION} -X github.com/rmotti/payments-boilerplate/internal/platform/buildinfo.Commit=${COMMIT} -X github.com/rmotti/payments-boilerplate/internal/platform/buildinfo.BuildTime=${BUILD_TIME}" \
    -o /out/migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/api /app/api
COPY --from=build /out/worker /app/worker
COPY --from=build /out/migrate /app/migrate
COPY db/migrations /app/db/migrations

EXPOSE 8080
USER nonroot:nonroot
CMD ["/app/api"]

