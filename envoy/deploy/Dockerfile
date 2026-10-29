# One image for the backend, the load clients and Aquifer (built from a
# pinned commit). ROLE picks what the container runs. Envoy runs from its
# own official image.
FROM golang:1.26-alpine AS build
RUN apk add --no-cache git
WORKDIR /src/envoy
COPY envoy/go.mod ./
COPY envoy/ ./
RUN CGO_ENABLED=0 go build -o /out/ ./cmd/...

ARG AQUIFER_REF=630501d
RUN git clone https://github.com/rjpruitt16/aquifer /aquifer \
 && cd /aquifer && git checkout "$AQUIFER_REF" \
 && CGO_ENABLED=0 go build -ldflags "-X main.commit=$AQUIFER_REF" -o /out/aquifer ./cmd/aquifer

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /out/ /usr/local/bin/
COPY envoy/deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
