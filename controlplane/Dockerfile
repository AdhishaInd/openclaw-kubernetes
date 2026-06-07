# syntax=docker/dockerfile:1
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /controlplane .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /controlplane /controlplane
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/controlplane"]
