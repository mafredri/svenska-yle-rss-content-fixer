ARG GO_VERSION=1.17
ARG DISTROLESS_VERSION=nonroot

FROM golang:${GO_VERSION}-bullseye AS builder

WORKDIR /src
COPY . /src
RUN go get -d -v ./... && go build

FROM gcr.io/distroless/base-debian11:${DISTROLESS_VERSION}
COPY --from=builder /src/svenska-yle-rss-content-fixer /usr/local/bin/svenska-yle-rss-content-fixer

EXPOSE 8080
USER nonroot:nonroot
CMD ["svenska-yle-rss-content-fixer"]
