# Builds the e2e test plugin image. It serves double duty:
#   /plugin/foo  -> read by the OCI fetcher (source.image + source.path)
#   /www/foo     -> served over HTTP by busybox httpd (source.url)
# Context is this directory: docker build test/e2e/testplugin
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/foo .

FROM busybox:1.36
COPY --from=build /out/foo /plugin/foo
COPY --from=build /out/foo /www/foo
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
