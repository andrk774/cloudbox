module github.com/andrk774/cloudbox

go 1.24

replace golang.org/x/net => github.com/golang/net v0.41.0

replace golang.org/x/crypto => github.com/golang/crypto v0.39.0

replace golang.org/x/text => github.com/golang/text v0.26.0

replace golang.org/x/sys => github.com/golang/sys v0.33.0

replace golang.org/x/term => github.com/golang/term v0.32.0

replace golang.org/x/sync => github.com/golang/sync v0.15.0

require (
	fyne.io/systray v1.11.0
	github.com/emersion/go-ical v0.0.0-20240127095438-fc1c9d8fb2b6
	github.com/emersion/go-imap v1.2.1
	github.com/emersion/go-message v0.18.2
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6
	github.com/emersion/go-smtp v0.24.0
	github.com/emersion/go-vcard v0.0.0-20230815062825-8fda7d206ec9
	github.com/godbus/dbus/v5 v5.1.0
	github.com/lib/pq v1.10.9
	golang.org/x/image v0.28.0
	golang.org/x/net v0.41.0
	golang.org/x/sys v0.33.0
)

require golang.org/x/text v0.26.0 // indirect

require (
	github.com/emersion/go-webdav v0.6.0
	github.com/teambition/rrule-go v1.8.2 // indirect
)

replace golang.org/x/image => github.com/golang/image v0.28.0
