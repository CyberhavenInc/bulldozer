FROM golang

# add static files
COPY ca-certificates.crt /etc/ssl/certs/

# add application files
COPY ./ $GOPATH/src/github.com/CyberhavenInc/bulldozer

# build
RUN go install github.com/CyberhavenInc/bulldozer

# run
ENTRYPOINT ["/go/bin/bulldozer"]
CMD ["server", "--config", "/secrets/bulldozer.yml"]
