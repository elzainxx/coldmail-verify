# The ZeroBounce STAND-IN (tests and CI only; there is no real vendor call in CI and no real key anywhere).
# See main.go for what it serves. Built by .github/workflows/ci.yml with `docker build --pull --no-cache`.
#
# Both bases are pinned by DIGEST, the repo's rule for every image (scripts/check-base-image-pinned.sh: a tag
# moves, a digest is a content address). The tag stays in front of the digest only so the pin can be read:
#   builder  golang:1.26.11-alpine3.24   (module go.mod says go 1.26.0; CI's setup-go is 1.26)
#   runtime  alpine:3.24                 (the same pin the DNS and implicit-MX fixtures run under)
# TestZB_StandInDockerfile_BasesPinnedByDigest fails if either FROM loses its digest.
FROM golang:1.26.11-alpine3.24@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
WORKDIR /src
# stdlib only: the program needs no go.mod of the repo's module, so none is copied. The one made here lives
# and dies inside the build stage (the repository must keep exactly ONE go.mod).
COPY main.go .
RUN go mod init zbstub \
 && CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags="-s -w" -o /out/zbstub .

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN adduser -D -u 10001 zbstub
USER 10001
COPY --from=build /out/zbstub /usr/local/bin/zbstub
ENV ZBSTUB_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/zbstub"]
