FROM golang:latest AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o /vocipher ./cmd/server/

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

RUN useradd -r -s /bin/false vocipher
WORKDIR /app

COPY --from=builder /vocipher .
COPY web/ web/

RUN mkdir -p /app/data && chown vocipher:vocipher /app/data

USER vocipher

EXPOSE 8090
EXPOSE 3478/udp

CMD ["./vocipher"]
