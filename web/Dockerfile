FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o terraria-web .

FROM alpine:3.20
WORKDIR /app
COPY --from=builder /app/terraria-web .
EXPOSE 4823
CMD ["./terraria-web"]
