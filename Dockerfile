FROM golang:latest AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o /vocala ./cmd/server/

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

RUN useradd -r -s /bin/false vocala
WORKDIR /app

COPY --from=builder /vocala .
COPY web/ web/

RUN mkdir -p /app/data && chown vocala:vocala /app/data

USER vocala

EXPOSE 8090
EXPOSE 3478/udp

CMD ["./vocala"]
