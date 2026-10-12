FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /notifier ./cmd/notifier
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /notifier /notifier
COPY --from=build --chown=nonroot:nonroot /data /data
ENV NOTIFIER_DB=/data/notifier.db
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/notifier"]
