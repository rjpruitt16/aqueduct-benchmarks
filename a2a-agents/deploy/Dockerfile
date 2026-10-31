# The pilot machine's image: every program in cmd/, Aquifer built from
# AQUIFER_REF, Postgres, Redis, Python for the tests and bubblewrap to
# isolate them. deploy/stack.sh runs one measured run inside it.
#
# Build from a2a-agents/: docker build -f deploy/Dockerfile --build-arg AQUIFER_REF=<commit> .
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/ ./cmd/...

# #130 (shared destination limits), unmerged at the time of the pilot.
ARG AQUIFER_REF=5981996
RUN git clone -q https://github.com/rjpruitt16/aquifer /aquifer \
 && cd /aquifer && git checkout -q "$AQUIFER_REF" \
 && git rev-parse --short HEAD > /out/aquifer-ref \
 && CGO_ENABLED=0 go build -o /out/aquifer ./cmd/aquifer

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl python3 postgresql redis-server bubblewrap util-linux procps jq \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /usr/local/bin/
RUN mv /usr/local/bin/aquifer-ref /usr/local/share/aquifer-ref
ARG IMAGE_REF=unknown
RUN echo "$IMAGE_REF" > /usr/local/share/image-ref
COPY deploy/stack.sh deploy/series.sh /usr/local/bin/
COPY ana/calibrate.py /usr/local/share/a2a/calibrate.py
# The controller (deploy/ctl.sh) runs the pilot from Fly: flyctl and the
# scripts, under /opt/a2a so pilot.sh writes results to /opt/a2a/results.
RUN curl -fsSL https://fly.io/install.sh | sh -s 0.4.122
COPY deploy/ctl.sh deploy/pilot.sh deploy/reap.sh /opt/a2a/deploy/
CMD ["sleep", "infinity"]
