module github.com/kayushkin/location-store

go 1.26.0

require (
	github.com/kayushkin/activity-store v0.0.0
	github.com/kayushkin/llm-bridge v0.0.0
	github.com/kayushkin/photo-store v0.0.0
	github.com/mattn/go-sqlite3 v1.14.37
	github.com/paulmach/osm v0.9.0
)

require (
	github.com/DataDog/czlib v0.0.0-20240814115052-86a9592b3985 // indirect
	github.com/paulmach/orb v0.12.0 // indirect
	github.com/paulmach/protoscan v0.2.1 // indirect
	go.mongodb.org/mongo-driver v1.17.4 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
)

replace (
	github.com/kayushkin/activity-store => ../activity-store
	github.com/kayushkin/llm-bridge => ../llm-bridge
	github.com/kayushkin/photo-store => ../photo-store
)
