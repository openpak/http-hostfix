FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/http-hostfix .
FROM scratch
LABEL org.opencontainers.image.source="https://github.com/openpak/http-hostfix"
COPY --from=build /out/http-hostfix /http-hostfix
USER 65532:65532
EXPOSE 21090
ENTRYPOINT ["/http-hostfix"]
