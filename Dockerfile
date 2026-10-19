# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build

ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/deepseek-search-mcp .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/deepseek-search-mcp /deepseek-search-mcp

EXPOSE 8080
USER nonroot:nonroot
ENV ADDR=":8080"

ENTRYPOINT ["/deepseek-search-mcp"]
