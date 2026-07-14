# mc-web: static landing page. stdlib-only, so no go.sum and a tiny static
# binary on distroless. Built and pushed as ghcr.io/palumacil/mc-web by CI.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mc-web .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/mc-web /mc-web
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/mc-web"]
