ARG GOARCH="amd64"

FROM golang:1.22 AS builder
# golang envs
ARG GOARCH="amd64"
ARG GOOS=linux
ENV CGO_ENABLED=0

WORKDIR /go/src/app
COPY . .
RUN go mod download
RUN CGO_ENABLED=0 go build -o /go/bin/plugin ./plugin/main.go 
RUN CGO_ENABLED=0 go build -o /go/bin/ifup ./ifup/main.go 
RUN CGO_ENABLED=0 go build -o /go/bin/ifnetns ./ifnetns/main.go 


FROM debian:bookworm
COPY --from=builder --chown=root:root /go/bin/ifup /opt/cdi/bin/ifup
COPY --from=builder --chown=root:root /go/bin/ifnetns /opt/cdi/bin/ifnetns
COPY --from=builder --chown=root:root /go/bin/plugin /plugin
CMD ["/plugin"]
