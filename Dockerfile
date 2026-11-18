# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/shadowcheck .

# Distroless ships the CA certificates required for the TLS calls to Twitch.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/shadowcheck /shadowcheck
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/shadowcheck"]
