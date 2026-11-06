package possessionstore

// The closed sets of words some fields take. GET /vocabulary serves them all,
// so a client or a model never keeps its own copy.

var PossessionStatuses = []string{"owned", "lent_out", "sold", "given_away", "lost", "discarded"}
var Conditions = []string{"new", "good", "worn", "damaged", "broken"}
var AcquisitionMethods = []string{"bought", "gift", "made", "inherited", "found"}

var GarmentCategories = []string{"top", "bottom", "one_piece", "outerwear", "footwear", "headwear", "accessory", "underwear", "sleepwear", "swimwear"}
var GarmentLayers = []string{"base", "mid", "outer"}
var Colors = []string{"black", "white", "gray", "charcoal", "navy", "blue", "light_blue", "teal", "green", "olive", "yellow", "gold", "orange", "red", "burgundy", "pink", "purple", "brown", "tan", "beige", "cream", "khaki", "silver", "multicolor"}
var Patterns = []string{"solid", "striped", "plaid", "checked", "graphic", "logo", "floral", "camo", "polka_dot", "geometric", "heathered", "tie_dye", "other"}
var Fits = []string{"slim", "regular", "relaxed", "oversized"}
var SizeSystems = []string{"letter", "numeric", "waist_inseam_inches", "neck_sleeve_inches", "us_shoe_men", "us_shoe_women", "eu_shoe", "one_size"}
var Seasons = []string{"spring", "summer", "fall", "winter"}

var ComputerFormFactors = []string{"desktop", "laptop", "server", "phone", "tablet", "smartwatch", "single_board", "console"}
var ComputerPartTypes = []string{"storage_drive", "graphics_card", "memory_module", "processor", "motherboard", "power_supply", "monitor", "keyboard", "mouse", "webcam", "dock", "network_device", "external_enclosure", "other"}
var AudioDeviceTypes = []string{"headphones", "earbuds", "speakers", "amplifier", "audio_interface", "microphone", "mixer", "effects_pedal", "converter", "turntable", "cable", "other"}
var VehicleTypes = []string{"car", "truck", "motorcycle", "bicycle", "scooter", "other"}

var OutfitStatuses = []string{"suggested", "kept", "rejected"}

var Marketplaces = []string{"amazon", "facebook_marketplace", "ebay", "in_store", "other"}
var PurchaseSources = []string{"amazon_export", "facebook_export", "email_receipt", "manual"}
var PurchaseLineDispositions = []string{"unreviewed", "possession", "consumable", "returned", "gift_for_someone_else"}

var PhotoReviewStatuses = []string{"queued", "reading", "reconciling", "waiting_for_owner", "done", "failed"}
var SightingAgreements = []string{"both_agree", "readers_differ", "one_reader_only"}
var SightingStatuses = []string{"proposed", "accepted", "rejected", "same_as_other_sighting"}
var ResolutionActions = []string{"accept", "same_as", "same_as_sighting", "reject", "keep", "amend"}
var QuestionPurposes = []string{"decision", "detail"}

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
		"outfit_statuses":            OutfitStatuses,
		"marketplaces":               Marketplaces,
		"purchase_sources":           PurchaseSources,
		"purchase_line_dispositions": PurchaseLineDispositions,
		"photo_review_statuses":      PhotoReviewStatuses,
		"sighting_agreements":        SightingAgreements,
		"sighting_statuses":          SightingStatuses,
		"question_purposes":          QuestionPurposes,
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
