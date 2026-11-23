# Built by goreleaser, which provides the prebuilt binary per platform.
# Locally: goreleaser release --snapshot --clean
FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/jmap-paperless /jmap-paperless
ENTRYPOINT ["/jmap-paperless"]
