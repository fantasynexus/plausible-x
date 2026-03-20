FROM golang:1.23-alpine AS build

WORKDIR /app

COPY go.mod main.go ./
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o plausible-provisioner .

FROM alpine:3.20

WORKDIR /app

COPY --from=build /app/plausible-provisioner /usr/local/bin/plausible-provisioner

EXPOSE 8080

CMD ["plausible-provisioner"]
