# go-garmin

`go-garmin` is a small, dependency-free Go client for reading Garmin Connect
activities. It targets the undocumented Garmin Connect API, which can change
at any time and is not affiliated with Garmin.

## Install

```sh
go get github.com/se0/go-garmin
```

## Usage

Log in with your Garmin email and password, or provide an existing Bearer
access token. Keep credentials and tokens out of source control and logs.

```go
client, err := garmin.Login(context.Background(), os.Getenv("GARMIN_EMAIL"), os.Getenv("GARMIN_PASSWORD"), nil)
if err != nil {
	log.Fatal(err)
}

activities, err := client.ListActivities(context.Background(), garmin.ListOptions{
	Limit:        20,
	ActivityType: "running",
})
```

The public API covers activity listing and pagination, count, full summary and
detail documents, and downloads in original/FIT archive, TCX, GPX, KML, and
CSV formats. Full activity documents are returned as `json.RawMessage` because
Garmin does not publish a stable schema for them.

`Login` implements Garmin's undocumented mobile SSO flow. For an account with
MFA, pass an `MFAProvider` that retrieves a one-time code. This flow is prone
to change or bot challenges; callers can instead retain and pass an existing
Bearer token with `NewClient`.

## Development

The module uses only the Go standard library.

```sh
go test ./...
go vet ./...
```

The tests are hermetic: they use `httptest` and require neither Garmin
credentials nor network access.

An opt-in live, read-only integration test validates SSO and the activity
endpoints. It is skipped by default:

```sh
GARMIN_EMAIL='...' GARMIN_PASSWORD='...' go test -run TestGarminIntegration
```

If MFA is enabled, also set `GARMIN_MFA_CODE` immediately before the command.

## Credits

This library was developed with Codex and is based on the approach and
authentication research in [python-garminconnect](https://github.com/cyberjunky/python-garminconnect).
