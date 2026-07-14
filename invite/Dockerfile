# mc-invite: OIDC-gated invite service. Pure-Go (pgx, go-oidc, gorcon all
# CGO-free) so it links static and runs on distroless; the committed *_templ.go
# files mean the build needs no templ CLI. ca-certificates (in distroless
# static:nonroot) cover outbound HTTPS to Mojang and Authentik; the tz database
# is embedded in the binary (time/tzdata). Published as ghcr.io/palumacil/
# mc-invite by CI.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mc-invite .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/mc-invite /mc-invite
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/mc-invite"]
