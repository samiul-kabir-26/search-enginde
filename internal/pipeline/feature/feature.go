// Package feature is the scorer feature catalog (docs/MODELS.md §4): the names a scorer may
// declare and where each value comes from.
package feature

// Source is where a feature value comes from.
type Source string

// Feature sources.
const (
	SourceRetrieval Source = "retrieval"
	SourcePolicy    Source = "policy"
	SourcePayload   Source = "payload"
	SourceItem      Source = "item_features"
	SourceVendor    Source = "vendor_features"
	SourceQuery     Source = "query"
	SourceUser      Source = "user_features"
)

// Feature names.
const (
	RRFScore             = "rrf_score"
	BM25Norm             = "bm25_norm"
	CosineText           = "cosine_text"
	CosineImage          = "cosine_image"
	RetrieverCount       = "retriever_count"
	PolicyBoost          = "policy_boost"
	PriceMinorLog        = "price_minor_log"
	Rating               = "rating"
	RatingCountLog       = "rating_count_log"
	ListingCompleteness  = "listing_completeness"
	ImageCount           = "image_count"
	AgeDays              = "age_days"
	CTR7d                = "ctr_7d"
	CTR30d               = "ctr_30d"
	CVR7d                = "cvr_7d"
	CVR30d               = "cvr_30d"
	Orders30dLog         = "orders_30d_log"
	VendorScore          = "vendor_score"
	VendorOnTimeDispatch = "vendor_on_time_dispatch_rate"
	VendorDisputeRate    = "vendor_dispute_rate"
	VendorResponseHours  = "vendor_response_hours"
	VendorVerified       = "vendor_verified"
	QueryCategoryMatch   = "query_category_match"
	UserCategoryAffinity = "user_category_affinity"
	UserPriceBandMatch   = "user_price_band_match"
)

// Catalog maps every known feature to its source. User features are only available in S6.
func Catalog() map[string]Source {
	return map[string]Source{
		RRFScore: SourceRetrieval, BM25Norm: SourceRetrieval, CosineText: SourceRetrieval,
		CosineImage: SourceRetrieval, RetrieverCount: SourceRetrieval,
		PolicyBoost:   SourcePolicy,
		PriceMinorLog: SourcePayload, Rating: SourcePayload, RatingCountLog: SourcePayload,
		ListingCompleteness: SourcePayload, ImageCount: SourcePayload, AgeDays: SourcePayload,
		CTR7d: SourceItem, CTR30d: SourceItem, CVR7d: SourceItem, CVR30d: SourceItem, Orders30dLog: SourceItem,
		VendorScore: SourceVendor, VendorOnTimeDispatch: SourceVendor, VendorDisputeRate: SourceVendor,
		VendorResponseHours: SourceVendor, VendorVerified: SourceVendor,
		QueryCategoryMatch:   SourceQuery,
		UserCategoryAffinity: SourceUser, UserPriceBandMatch: SourceUser,
	}
}
