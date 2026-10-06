FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/whatsapp-mcp-server . && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/whatsapp-mcp-server /whatsapp-mcp-server
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME /data
EXPOSE 8080
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/whatsapp-mcp-server", "-healthcheck"]
ENTRYPOINT ["/whatsapp-mcp-server"]
