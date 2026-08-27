# go-garminconnect

`go-garminconnect` is a small, dependency-free Go client for reading Garmin
Connect activities. It targets the undocumented Garmin Connect API, which can
change at any time and is not affiliated with Garmin.

## Install

```sh
go get github.com/se0wtf/go-garminconnect
```

## Usage

Log in with your Garmin email and password, or resume a session from a secure
token file. Keep credentials and tokens out of source control and logs.

```go
tokenFile := filepath.Join(configDir, "go-garminconnect", "tokens.json")
client, err := garmin.NewClientFromTokenFile(tokenFile)
if errors.Is(err, garmin.ErrNoTokenFile) || errors.Is(err, garmin.ErrSessionExpired) {
	client, err = garmin.Login(
		context.Background(),
		os.Getenv("GARMIN_EMAIL"),
		os.Getenv("GARMIN_PASSWORD"),
		nil,
		garmin.WithTokenFile(tokenFile),
	)
}
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
CSV formats. `GetDiveDetails` retrieves Garmin Dive-specific data. Full detail
documents are returned as `json.RawMessage` because Garmin does not publish a
stable schema for them.

`Login` uses Garmin's undocumented browser SSO flow, with the mobile flow as a
fallback. For an account with MFA, pass an `MFAProvider` that retrieves a
one-time code. `WithTokenFile` stores API tokens and the authenticated browser
session with owner-only permissions. Clients resumed with
`NewClientFromTokenFile` refresh expiring API tokens automatically and can call
authenticated browser-only endpoints such as `GetDiveDetails` without logging
in again. If Garmin expires the browser session, `GetDiveDetails` returns
`ErrSessionExpired` and the application must call `Login` again.

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
