package keeper

// ObsLink is one observability UI entry: the backend's name,
// its configured base URL, and what it is for. The blurbs are
// stack facts (what each backend keeps), so they live here
// beside the assembly — both renderers voice the same words.
type ObsLink struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Blurb string `json:"blurb,omitempty"`
}

// ObsCard voices the configured observability backends for the
// console launchpad and `status observability`. Standing voices
// the wiring, never the backends' health — an empty card reads
// unavailable (nothing configured), a wired one ok.
type ObsCard struct {
	Standing Standing  `json:"standing"`
	Links    []ObsLink `json:"links,omitempty"`
}

// ObsURLs carries the configured UI base URLs in: empty means
// the backend is absent and stays out of the card.
type ObsURLs struct {
	Quickwit   string
	Jaeger     string
	Grafana    string
	Prometheus string
}

// FetchObsCard resolves the configured URLs into launchpad
// entries in launchpad order, skipping every backend without
// one. No probing — configured is the whole fact; whether the
// backend answers is the backend's own story to tell.
func FetchObsCard(urls ObsURLs) ObsCard {
	var links []ObsLink
	add := func(url, name, blurb string) {
		if url == "" {
			return
		}
		links = append(links, ObsLink{Name: name, URL: url, Blurb: blurb})
	}
	add(urls.Quickwit, "Quickwit", "Searchable logs, kept across dev runs")
	add(urls.Jaeger, "Jaeger", "Distributed traces, stored in Quickwit")
	add(urls.Grafana, "Grafana", "Metrics dashboards for this service")
	add(urls.Prometheus, "Prometheus", "Raw metrics and scrape targets")
	if len(links) == 0 {
		return ObsCard{Standing: StandingUnavailable}
	}
	return ObsCard{Standing: StandingOK, Links: links}
}
