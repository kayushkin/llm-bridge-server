# location-store

Keeps where you have been: named places, visits to them, trips between them,
and the raw location points underneath. It imports Google's location history
(the 2024 on-device Timeline export and both legacy Takeout formats) and the
GPS of photos held by photo-store.

Each place, visit and trip has an id like `trip_000001`, so a note or a
journal timeline can cite a particular drive, and bridge-ui draws that id as
a chip, using the TypeScript types in `ts/`.

## Where it runs

location-store runs inside llm-bridge-server, at `/locations/`, with
the same paths it served as a standalone service on `:8333`. Its records
are in Postgres, in the schema `locations`, which the role
`locations_module` owns (`Module()`); the server provisions and migrates it
and hands the pool to `New(db, geonamesDirectory)`. The GeoNames directory
is the server's `locations.geonames_directory` setting: the store reads
the town files from there, and answers 409 on the passes that need them
until they are there.

`CONTRACT.md` lists every route and field.

## Owners

Every record has an owner, `owner_principal_id`, principal-store's id of the
person who keeps it. A caller reads and changes only their own records; an
administrator sees every owner's and can narrow to one with
`?owner_principal_id=`. Records of two owners are never joined: a visit
cannot sit at another owner's place, nor a trip inside another owner's
journey. Two people may each import the same file. CONTRACT.md *Owners* has
the whole rule.

## What it does

- **Import** Google location history. Each file is found again among its
  owner's imports by its SHA-256, so importing it twice changes nothing, and
  an import that stopped part way resumes when run again.
- **Group trips.** Make a trip by hand for a road trip or a holiday and
  attach the drives and flights it was made of.
- **Describe** places, visits and trips, with who wrote the description. An
  import never overwrites a name, title, description, tags or grouping
  someone wrote.
- **Keep history.** Every change copies the whole record to a revision first,
  and deletes are soft.
- **Read** a day as visits and trips in order (`GET /timeline`), or the raw
  points of any time range (`GET /points`).

## Commands

The commands in `cmd/` are HTTP clients of the routes, each with its own
settings (CONTRACT.md *Commands' settings*):

```sh
location-import-google -file Timeline.json -dry-run     # say what it would read
location-import-google -file Timeline.json
location-import-google -file "Takeout/Location History"  # Records.json and every month file
location-import-photos                                   # photo GPS from photo-store
location-name-localities                                 # needs LOCATION_STORE_GEONAMES_DIRECTORY
```

`location-name-localities` downloads the GeoNames files to
`LOCATION_STORE_GEONAMES_DIRECTORY`, which must name the same directory as
the server's `locations.geonames_directory`.

## What changed in the move

- SQLite became Postgres; ids, times, JSON fields and answers are as they
  were, and every record with an id gains `owner_principal_id`.
- `GET /settings`, the listen address and the data directory setting are
  gone with the standalone server.
- `?q=` on `/places` still matches each word anywhere in the name, address,
  description or tags, ignoring the case of A to Z only, as SQLite's `LIKE`
  did. Tags are now `jsonb`, so a word is matched against their text as
  Postgres writes it (`["a", "b"]`, characters unescaped).
- A number is never handed out twice, so a dry run that would create
  records takes numbers it gives back, and the real run after it gets later
  ones.

## License

MIT. See `LICENSE`.
