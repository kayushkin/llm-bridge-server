# location-store

A small Go service that keeps where you have been: named places, visits to
them, trips between them, and the raw location points underneath, in SQLite
behind a loopback HTTP API. It imports Google's location history (the 2024
on-device Timeline export and both legacy Takeout formats) and the GPS of
photos held by [photo-store](https://github.com/kayushkin/photo-store).

It is one of the stores of the [llm-bridge](https://github.com/kayushkin/llm-bridge)
ecosystem. Each place, visit and trip has an id like `trip_000001`, so a note
or a journal timeline can cite a particular drive, and
[bridge-ui](https://github.com/kayushkin/bridge-ui) draws that id as a chip,
using the TypeScript types in `ts/`. It works on its own too: it is a plain
HTTP API.

## What it does

- **Import** Google location history. Each file is found again by its
  SHA-256, so importing it twice changes nothing, and an import that stopped
  part way resumes when run again.
- **Group trips.** Make a trip by hand for a road trip or a holiday and
  attach the drives and flights it was made of.
- **Describe** places, visits and trips, with who wrote the description. An
  import never overwrites a name, title, description, tags or grouping
  someone wrote.
- **Keep history.** Every change copies the whole record to a revision first,
  and deletes are soft.
- **Read** a day as visits and trips in order (`GET /timeline`), or the raw
  points of any time range (`GET /points`).

`CONTRACT.md` lists every route and field.

## Build

location-store reads its settings through llm-bridge's `servicesettings`
package, and its photo importer uses photo-store's types, so `go.mod` points
at checkouts beside this one:

```sh
git clone https://github.com/kayushkin/llm-bridge
git clone https://github.com/kayushkin/photo-store
git clone https://github.com/kayushkin/location-store
cd location-store
go test ./...
go build ./cmd/location-store ./cmd/location-import-google ./cmd/location-import-photos
```

## Run

```sh
./location-store            # listens on 127.0.0.1:8333
curl -s localhost:8333/health
```

| Variable | Default | |
|---|---|---|
| `LOCATION_STORE_ADDR` | `127.0.0.1:8333` | listen address |
| `LOCATION_STORE_DATA_DIR` | `~/.config/location-store` | the database |

`GET /settings` describes every setting and the value in force.

**There is no authentication.** Keep it on loopback, as the default does, and
put a front door that checks who is asking in front of it if anyone else
should reach it.

To run it as a systemd user service, copy the binary to `~/bin` and
`location-store.service` to `~/.config/systemd/user/`, then
`systemctl --user enable --now location-store`.

## Importing

```sh
location-import-google -file Timeline.json -dry-run     # say what it would read
location-import-google -file Timeline.json
location-import-google -file "Takeout/Location History"  # Records.json and every month file
location-import-photos                                   # photo GPS from photo-store
```

Both commands post to `LOCATION_STORE_URL` (default `http://127.0.0.1:8333`);
`location-import-photos` reads `PHOTO_STORE_URL` (default
`http://127.0.0.1:8329`).

## License

MIT. See `LICENSE`.
