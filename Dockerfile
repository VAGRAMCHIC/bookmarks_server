# SPDX-License-Identifier: AGPL-3.0-or-later

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/bookmarks-server ./cmd/bookmarks-server \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bookmarks-server /usr/local/bin/bookmarks-server
# The volume inherits this owner, so the nonroot user can write to it.
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV BOOKMARKS_DATA=/data
VOLUME /data
EXPOSE 8443
USER nonroot
ENTRYPOINT ["/usr/local/bin/bookmarks-server"]
CMD ["serve"]
