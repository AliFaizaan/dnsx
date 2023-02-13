FROM golang:1.18.1-alpine3.14 AS build-env
RUN go install -v github.com/projectdiscovery/dnsx/cmd/dnsx@latest

FROM alpine:3.17.2
RUN apk add --no-cache bind-tools ca-certificates
COPY --from=build-env /go/bin/dnsx /usr/local/bin/dnsx
ENTRYPOINT ["dnsx"]
