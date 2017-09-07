FROM sergef/docker-library-alpine:edge

ENV HOST_IP 0.0.0.0
ENV CONSUL_HOST consul:8500

ENV GOPATH /go

COPY entrypoint.sh /entrypoint.sh
COPY registrator.go /go/src/github.com/sergef/docker-library-registrator/

RUN apk add \
    --no-cache \
    gcc \
    g++ \
    go@community \
    git \
    mercurial \
  && cd /go/src/github.com/sergef/docker-library-registrator \
  && go get \
  && go build -o /bin/registrator \
  && apk del \
    --no-cache \
    go \
    gcc \
    g++ \
    git \
    mercurial \
  && chmod +x /entrypoint.sh \
  && rm -rf \
    /go \
    /var/cache/apk/*

ENTRYPOINT ["tini", "--", "/entrypoint.sh"]
