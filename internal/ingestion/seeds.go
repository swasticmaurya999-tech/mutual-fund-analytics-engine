package ingestion

// TrackedSchemes contains the 10 scheme codes this service monitors.
// 5 AMCs × 2 categories: Equity Mid Cap Direct Growth + Equity Small Cap Direct Growth.
// All scheme metadata is fetched live from GET /mf/{code} on first sync.
//
// Codes verified via GET https://api.mfapi.in/mf/{code}/latest on 2026-04-18.
var TrackedSchemes = []string{
	// Equity Scheme — Mid Cap Fund — Direct Plan — Growth
	"120505", // Axis Mutual Fund
	"118989", // HDFC Mutual Fund
	"120381", // ICICI Prudential Mutual Fund
	"119716", // SBI Mutual Fund
	"119775", // Kotak Mahindra Mutual Fund

	// Equity Scheme — Small Cap Fund — Direct Plan — Growth
	"125354", // Axis Mutual Fund
	"130503", // HDFC Mutual Fund
	"120591", // ICICI Prudential Mutual Fund
	"125497", // SBI Mutual Fund
	"120164", // Kotak Mahindra Mutual Fund
}
