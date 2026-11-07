package possessionstore

// The closed sets of words some fields take. GET /vocabulary serves them all,
// so a client or a model never keeps its own copy.

// unconfirmed: seen in a photo of the owner wearing or using it, but not yet
// confirmed as still owned.
var PossessionStatuses = []string{"owned", "unconfirmed", "lent_out", "sold", "given_away", "lost", "discarded"}
var Conditions = []string{"new", "good", "worn", "damaged", "broken"}
var AcquisitionMethods = []string{"bought", "gift", "made", "inherited", "found"}

var GarmentCategories = []string{"top", "bottom", "one_piece", "outerwear", "footwear", "headwear", "accessory", "underwear", "sleepwear", "swimwear"}
var GarmentTypes = []string{
	"t_shirt", "tank_top", "polo", "button_up_shirt", "flannel_shirt", "henley", "long_sleeve_shirt", "sweater", "cardigan", "hoodie", "sweatshirt", "vest", "blouse",
	"jeans", "chinos", "trousers", "sweatpants", "joggers", "shorts", "skirt", "leggings",
	"dress", "jumpsuit", "suit", "overalls",
	"jacket", "denim_jacket", "leather_jacket", "varsity_jacket", "bomber_jacket", "blazer", "coat", "parka", "puffer", "raincoat", "windbreaker",
	"sneakers", "running_shoes", "boots", "dress_shoes", "loafers", "sandals", "slippers", "skates",
	"baseball_cap", "beanie", "hat", "headphones_worn",
	"belt", "scarf", "gloves", "tie", "socks",
	"underwear", "pajamas", "swimsuit", "other"}
var GarmentLayers = []string{"base", "mid", "outer"}
var Colors = []string{"black", "white", "gray", "charcoal", "navy", "blue", "light_blue", "teal", "green", "olive", "yellow", "gold", "orange", "red", "burgundy", "pink", "purple", "brown", "tan", "beige", "cream", "khaki", "silver", "multicolor"}
var Patterns = []string{"solid", "striped", "plaid", "checked", "graphic", "logo", "floral", "camo", "polka_dot", "geometric", "heathered", "tie_dye", "other"}
var Fits = []string{"slim", "regular", "relaxed", "oversized"}
var SizeSystems = []string{"letter", "numeric", "waist_inseam_inches", "neck_sleeve_inches", "us_shoe_men", "us_shoe_women", "eu_shoe", "one_size"}
var Seasons = []string{"spring", "summer", "fall", "winter"}

var JewelryTypes = []string{"necklace", "pendant", "chain", "ring", "bracelet", "earring", "anklet", "brooch", "cufflinks", "other"}
var Metals = []string{"gold", "silver", "sterling_silver", "stainless_steel", "titanium", "tungsten", "platinum", "brass", "copper", "leather", "cord", "other"}
var WatchMovements = []string{"automatic", "manual", "quartz", "solar", "smart", "other"}
var EyewearTypes = []string{"sunglasses", "prescription", "reading", "blue_light", "safety", "ski_goggles", "other"}
var ComputerFormFactors = []string{"desktop", "laptop", "server", "phone", "tablet", "smartwatch", "single_board", "console", "vr_headset"}
var ComputerPartTypes = []string{"storage_drive", "graphics_card", "memory_module", "processor", "motherboard", "power_supply", "monitor", "keyboard", "mouse", "webcam", "dock", "network_device", "external_enclosure", "other"}
var AudioDeviceTypes = []string{"headphones", "earbuds", "speakers", "amplifier", "audio_interface", "microphone", "mixer", "effects_pedal", "converter", "turntable", "cable", "other"}
var VehicleTypes = []string{"car", "truck", "motorcycle", "bicycle", "scooter", "other"}

var Rooms = []string{"bedroom", "living_room", "office", "kitchen", "dining_room", "bathroom", "hallway", "entryway", "closet", "laundry", "garage", "storage", "outdoors", "other"}
var FurnitureTypes = []string{"bed", "desk", "chair", "office_chair", "sofa", "armchair", "table", "side_table", "coffee_table", "dining_table", "bookcase", "shelving", "cabinet", "dresser", "nightstand", "wardrobe", "stool", "footstool", "bench", "other"}
var FitnessEquipmentTypes = []string{"weight_plate", "barbell", "curl_bar", "dumbbell", "kettlebell", "power_rack", "weight_bench", "exercise_mat", "resistance_band", "other"}
var Sports = []string{"skiing", "snowboarding", "ice_skating", "disc_golf", "cycling", "climbing", "hiking", "camping", "swimming", "running", "ball_sports", "other"}
var TabletopGameTypes = []string{"board_game", "card_game", "jigsaw_puzzle", "dice", "puzzle_toy", "other"}
var MediaTypes = []string{"book", "magazine", "comic", "vinyl_record", "compact_disc", "video_game", "film", "sheet_music", "other"}
var CollectibleTypes = []string{"figure", "plush", "replica", "medal", "trading_card", "graded_game", "pin", "other"}
var DecorTypes = []string{"framed_art", "poster", "painting", "print", "vase", "sign", "plant", "lamp", "mirror", "sculpture", "tray", "clock", "other"}
var HomeTextileTypes = []string{"blanket", "throw", "pillow", "cushion", "comforter", "sheet", "towel", "curtain", "rug", "mat", "other"}
var ApplianceTypes = []string{"refrigerator", "freezer", "air_conditioner", "fan", "heater", "microwave", "oven", "dishwasher", "washer", "dryer", "vacuum", "humidifier", "air_purifier", "coffee_maker", "other"}
var PersonalCareTypes = []string{"toothbrush", "water_flosser", "trimmer", "shaver", "hair_dryer", "comb", "brush", "nail_clipper", "contact_lens_case", "thermometer", "other"}
var BagTypes = []string{"backpack", "tote", "duffel", "sports_bag", "carrying_case", "instrument_case", "laptop_bag", "suitcase", "pouch", "other"}
var ElectronicAccessoryTypes = []string{"cable", "power_strip", "adapter", "charger", "stand", "mount", "arm", "hub", "battery", "other"}
var HouseholdToolTypes = []string{"hand_tool", "power_tool", "cleaning", "laundry", "storage", "kitchen", "garden", "other"}
var MusicAccessoryTypes = []string{"stand", "bench", "throne", "pedal", "strap", "capo", "tuner", "metronome", "music_books", "other"}

// WearableKinds are the kinds an outfit may hold.
var WearableKinds = []string{"garment", "jewelry", "watch", "eyewear", "bag_or_case"}

var OutfitStatuses = []string{"suggested", "kept", "rejected"}

var Marketplaces = []string{"amazon", "facebook_marketplace", "ebay", "in_store", "other"}
var PurchaseSources = []string{"amazon_export", "facebook_export", "email_receipt", "manual"}
var PurchaseLineDispositions = []string{"unreviewed", "possession", "consumable", "returned", "gift_for_someone_else"}

// inventory: two readers list everything in a photo of the home. outfit: one
// reader lists what the wearer has on, and the outfit is recorded.
var PhotoReviewReadings = []string{"inventory", "outfit"}
var PhotoReviewStatuses = []string{"queued", "reading", "reconciling", "waiting_for_owner", "done", "failed"}
var SightingAgreements = []string{"both_agree", "readers_differ", "one_reader_only", "single_reader"}
var SightingStatuses = []string{"proposed", "accepted", "rejected", "same_as_other_sighting"}

// keep_link, merge_possessions, keep_apart, combine_outfits and
// keep_outfits_apart answer the wearable audit's questions (auditActions).
var ResolutionActions = []string{"accept", "same_as", "same_as_sighting", "reject", "keep", "amend",
	"keep_link", "merge_possessions", "keep_apart", "combine_outfits", "keep_outfits_apart"}
var QuestionPurposes = []string{"decision", "detail"}

// What raised a question other than the photo's own reading.
var QuestionAskers = []string{"link_check", "duplicate_check", "outfit_consolidation", "picture_match"}

// Vocabulary is GET /vocabulary.
func Vocabulary() map[string][]string {
	return map[string][]string{
		"possession_statuses":        PossessionStatuses,
		"conditions":                 Conditions,
		"acquisition_methods":        AcquisitionMethods,
		"kinds":                      KindNames(),
		"garment_categories":         GarmentCategories,
		"garment_layers":             GarmentLayers,
		"colors":                     Colors,
		"patterns":                   Patterns,
		"fits":                       Fits,
		"size_systems":               SizeSystems,
		"seasons":                    Seasons,
		"computer_form_factors":      ComputerFormFactors,
		"computer_part_types":        ComputerPartTypes,
		"audio_device_types":         AudioDeviceTypes,
		"vehicle_types":              VehicleTypes,
		"rooms":                      Rooms,
		"furniture_types":            FurnitureTypes,
		"fitness_equipment_types":    FitnessEquipmentTypes,
		"sports":                     Sports,
		"tabletop_game_types":        TabletopGameTypes,
		"media_types":                MediaTypes,
		"collectible_types":          CollectibleTypes,
		"decor_types":                DecorTypes,
		"home_textile_types":         HomeTextileTypes,
		"appliance_types":            ApplianceTypes,
		"personal_care_types":        PersonalCareTypes,
		"bag_types":                  BagTypes,
		"electronic_accessory_types": ElectronicAccessoryTypes,
		"household_tool_types":       HouseholdToolTypes,
		"music_accessory_types":      MusicAccessoryTypes,
		"garment_types":              GarmentTypes,
		"jewelry_types":              JewelryTypes,
		"metals":                     Metals,
		"watch_movements":            WatchMovements,
		"eyewear_types":              EyewearTypes,
		"photo_review_readings":      PhotoReviewReadings,
		"wearable_kinds":             WearableKinds,
		"outfit_statuses":            OutfitStatuses,
		"marketplaces":               Marketplaces,
		"purchase_sources":           PurchaseSources,
		"purchase_line_dispositions": PurchaseLineDispositions,
		"photo_review_statuses":      PhotoReviewStatuses,
		"sighting_agreements":        SightingAgreements,
		"sighting_statuses":          SightingStatuses,
		"question_purposes":          QuestionPurposes,
		"question_askers":            QuestionAskers,
		"resolution_actions":         ResolutionActions,
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
