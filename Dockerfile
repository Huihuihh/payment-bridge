FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/payment-bridge .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/payment-bridge /usr/local/bin/payment-bridge
EXPOSE 8787
ENTRYPOINT ["payment-bridge"]
