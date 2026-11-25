package config

import (
	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of possession-store's settings in this server. The store keeps its
// records in its module of database.postgres_url and its originals' bytes in
// the file-store module. Each is read once, at start.
//
// The photo review, and the background reading of outfit covers' wearer
// boxes, spend money on model calls, so they are off until
// possession_store.review_worker.enabled turns them on, and even then run
// only for the owners named in possession_store.review_worker.owner_principal_ids,
// each as that owner.
const (
	SettingPossessionStoreReviewWorkerEnabled           = "possession_store.review_worker.enabled"
	SettingPossessionStoreReviewWorkerOwnerPrincipalIDs = "possession_store.review_worker.owner_principal_ids"
	SettingPossessionStoreFirstReaderRole               = "possession_store.first_reader_role"
	SettingPossessionStoreSecondReaderRole              = "possession_store.second_reader_role"
	SettingPossessionStoreOwnerTimeZone                 = "possession_store.owner_time_zone"
	SettingPossessionStoreReviewImageLongestSide        = "possession_store.review_image_longest_side"
	SettingPossessionStoreCoverCacheDirectory           = "possession_store.cover_cache_directory"
	SettingPossessionStoreArticleStoreURL               = "possession_store.article_store_url"
)

// possessionsSettingDefinitions declares the settings above.
func possessionsSettingDefinitions() []servicesettings.Definition {
	const (
		wiring   = msg.ServiceSettingKindWiring
		path     = msg.ServiceSettingKindPath
		behavior = msg.ServiceSettingKindBehavior
		text     = msg.ServiceSettingValueTypeString
		list     = msg.ServiceSettingValueTypeStringList
		integer  = msg.ServiceSettingValueTypeInteger
		boolean  = msg.ServiceSettingValueTypeBoolean
		role     = msg.ServiceSettingValueTypeModelRole
	)
	return []servicesettings.Definition{
		{Key: SettingPossessionStoreReviewWorkerEnabled, EnvironmentVariable: "LLMBRIDGE_POSSESSION_STORE_REVIEW_WORKER_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Review the photos queued for possession-store with its two readers, one at a time, and read the wearer box of outfit covers the outfit list shows unread. Both are model calls through this server's one-shot router. Off by default, while the standalone possession-store still runs its own worker; off, queued photos wait. Read at start."},
		{Key: SettingPossessionStoreReviewWorkerOwnerPrincipalIDs, EnvironmentVariable: "LLMBRIDGE_POSSESSION_STORE_REVIEW_WORKER_OWNER_PRINCIPAL_IDS", Kind: behavior, ValueType: list, Default: "",
			Description: "The principals whose queued photos the review worker reads, each as that owner, so one owner's photos are never matched against another's inventory. Empty reviews nobody's. Read at start."},
		{Key: SettingPossessionStoreFirstReaderRole, EnvironmentVariable: "LLMBRIDGE_POSSESSION_STORE_FIRST_READER_ROLE", Kind: behavior, ValueType: role, Default: "possession-photo-reader",
			Description: "The model-store role of possession-store's first photo reader. It also turns an answer the owner wrote in words into a decision, reads outfits, product listings and eBay mail. Read at start."},
		{Key: SettingPossessionStoreSecondReaderRole, EnvironmentVariable: "LLMBRIDGE_POSSESSION_STORE_SECOND_READER_ROLE", Kind: behavior, ValueType: role, Default: "possession-photo-second-reader",
			Description: "The model-store role of possession-store's second photo reader, which must agree with the first. Read at start."},
		{Key: SettingPossessionStoreOwnerTimeZone, EnvironmentVariable: "LLMBRIDGE_POSSESSION_STORE_OWNER_TIME_ZONE", Kind: behavior, ValueType: text, Default: "UTC",
			Description: "The IANA time zone an outfit read from a photo is recorded as worn in: the photo's day there. Read at start."},
		{Key: SettingPossessionStoreReviewImageLongestSide, EnvironmentVariable: "LLMBRIDGE_POSSESSION_STORE_REVIEW_IMAGE_LONGEST_SIDE", Kind: behavior, ValueType: integer, Default: "2048",
			Description: "The longest side, in pixels, of the photo the readers are shown when the owner keeps its original. Without one they see photo-store's 1024-pixel thumbnail. Read at start."},
		{Key: SettingPossessionStoreCoverCacheDirectory, EnvironmentVariable: "LLMBRIDGE_POSSESSION_STORE_COVER_CACHE_DIRECTORY", Kind: path, ValueType: text, Default: productiondefaults.PossessionStoreCoverCacheDirectory(),
			Description: "Where outfit covers already cut are kept, since cutting a full-size original takes about a second. Every file there may be deleted; it is cut again when asked for. Read at start."},
		{Key: SettingPossessionStoreArticleStoreURL, EnvironmentVariable: "LLMBRIDGE_POSSESSION_STORE_ARTICLE_STORE_URL", Kind: wiring, ValueType: text, Default: "",
			Description: "Where article-store answers, outside this server: a product's listing page is saved and read there, and a product's article_id checked there. Empty, those answer that it is not set. Read at start."},
	}
}
