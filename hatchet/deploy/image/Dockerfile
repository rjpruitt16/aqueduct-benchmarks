# One image for every benchmark role: the pipeline worker, the driver, the
# Postgres sampler and Aquifer (built from a pinned commit). ROLE picks what
# the container runs; see entrypoint.sh.
FROM golang:1.26-alpine AS build
RUN apk add --no-cache git
WORKDIR /src/hatchet
COPY hatchet/go.mod hatchet/go.sum ./
RUN go mod download
COPY hatchet/ ./
RUN CGO_ENABLED=0 go build -o /out/ ./cmd/...

ARG AQUIFER_REF=e283a06
RUN git clone https://github.com/rjpruitt16/aquifer /aquifer \
 && cd /aquifer && git checkout "$AQUIFER_REF" \
 && CGO_ENABLED=0 go build -ldflags "-X main.commit=$AQUIFER_REF" -o /out/aquifer ./cmd/aquifer

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /out/ /usr/local/bin/
COPY hatchet/deploy/image/entrypoint.sh /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
