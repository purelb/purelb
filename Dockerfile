FROM golang:1.18.4-bullseye as builder

ARG branch
ARG commit
ARG release=development
ENV GOOS=linux

WORKDIR /usr/src
COPY . ./

# build the executables
RUN go build -tags 'osusergo netgo' -o /tmp/allocator \
-ldflags "-X purelb.io/internal/logging.release=${release} -X purelb.io/internal/logging.commit=${commit} -X purelb.io/internal/logging.branch=${branch}" \
./cmd/allocator/
RUN go build -tags 'osusergo netgo' -o /tmp/lbnodeagent \
-ldflags "-X purelb.io/internal/logging.release=${release} -X purelb.io/internal/logging.commit=${commit} -X purelb.io/internal/logging.branch=${branch}" \
./cmd/lbnodeagent/

FROM ubuntu:20.04 as runtime
ARG branch
ARG commit

LABEL branch=${branch}
LABEL commit=${commit}

# The egress announcer needs iptables
RUN apt-get update && apt-get install -y iptables

COPY --from=builder /tmp/allocator /usr/local/bin/allocator
COPY --from=builder /tmp/lbnodeagent /usr/local/bin/lbnodeagent
