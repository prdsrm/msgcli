# msgcli MCP server: read-only Outlook mail over MCP Streamable HTTP,
# behind Microsoft Entra ID sign-in. See README "MCP server".
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/msgcli ./cmd/msgcli

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/msgcli /usr/local/bin/msgcli
ENV MCP_LISTEN_ADDR=:8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/msgcli", "mcp", "serve"]
