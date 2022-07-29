FROM golang:1.19-bullseye as builder

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

FROM registry.access.redhat.com/ubi8-minimal:8.7-1085 as runtime
ARG branch
ARG commit

LABEL branch=${branch}
LABEL commit=${commit}

COPY --from=builder /tmp/allocator /usr/local/bin/allocator
COPY --from=builder /tmp/lbnodeagent /usr/local/bin/lbnodeagent
