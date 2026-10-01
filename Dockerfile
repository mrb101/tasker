# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/tasker ./cmd/tasker \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tasker /usr/local/bin/tasker
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV TASKER_DB=/data/tasker.db TASKER_ADDR=:7777 TASKER_URL=http://localhost:7777
VOLUME ["/data"]
EXPOSE 7777
HEALTHCHECK --interval=30s --timeout=3s CMD ["tasker", "healthcheck"]
ENTRYPOINT ["tasker"]
CMD ["serve"]
