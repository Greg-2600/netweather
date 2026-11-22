package web

import (
	"math"
	"sort"
)

// mapTile is one target on the /map treemap.
type mapTile struct {
	Label  string
	Ticker string // compact label for small tiles
	Target string
	State  string
	Group  string
	Color  string
	// Detail is the error class (or the state when there is none); Duration is
	// the human-readable time the target has been in this state. Both are
	// surfaced because severity and duration are what make an outage urgent.
	Detail    string
	Duration  string
	DurationS float64 // seconds in state, used to rank capped-outages by age
	// Kind, Endpoint, Why, StateLabel and Tip feed the hover card. They explain
	// what the target is and why the classifier gave it this status.
	Kind       string
	Endpoint   string
	Why        string
	StateLabel string
	Tip        string
	Up         int
	Down       int
	// Weight is the layout area of the tile. It grows with severity and with
	// the time spent in a bad state, so an outage dominates the map instead
	// of being lost among hundreds of healthy targets.
	Weight int
	// ShowFull / ShowTicker / ShowSub gate text by the tile's rendered size,
	// so labels are never crammed into a box too small to display them.
	ShowFull   bool
	ShowTicker bool
	ShowSub    bool
	// X, Y, W, H are percentages local to the tile's sector block.
	X, Y, W, H float64
}

// mapSector is a cluster of targets on the /map treemap. Sectors are laid out
// as contiguous blocks first, then their tiles are packed inside the block.
// X, Y, W, H are percentages of the whole treemap.
type mapSector struct {
	Label string
	Count int
	Color string
	Up    int
	Down  int
	// Area is the layout weight of the cluster (sum of its tile weights). It
	// falls back to Count when unset, which keeps the non-health modes honest.
	Area float64
	// Order places a cluster in the layout: lower is more urgent and is packed
	// first (top-left), so outages are seen before healthy targets.
	Order int
	// ShowHead reports whether the block is large enough to carry a header.
	// Tiny blocks spend every pixel on tiles instead of an unreadable label.
	ShowHead   bool
	X, Y, W, H float64
	Tiles      []*mapTile

	// Hub presentation, empty on /map. The hubs view reuses the treemap as a
	// city -> sites map: the sector is one city, and each tile is one endpoint.
	// These fields carry the name-convention metadata and the drill-down link
	// without adding a first-class location field to the stored schema.
	City     string
	State    string
	Href     string
	Anchor   string // stable fragment id for the city block, linked from the map
	Worst    string
	WorstCls string
	Glyph    string
}

type sqItem struct {
	tile *mapTile
	area float64
}

// tileArea is the layout area of a tile: its explicit severity/duration
// weight when set, otherwise one unit per target.
func tileArea(t *mapTile) float64 {
	if t.Weight > 0 {
		return float64(t.Weight)
	}
	return 1
}

// sectorArea is the layout area of a cluster.
func sectorArea(s *mapSector) float64 {
	if s.Area > 0 {
		return s.Area
	}
	if s.Count > 0 {
		return float64(s.Count)
	}
	return 1
}

// layoutSectors runs a squarified treemap over sectors. Clusters are packed in
// Order first (urgent clusters first) and by area within the same order.
func layoutSectors(sectors []*mapSector, w, h float64) {
	if len(sectors) == 0 {
		return
	}
	total := 0.0
	for _, s := range sectors {
		total += sectorArea(s)
	}
	if total <= 0 {
		return
	}
	area := w * h
	items := make([]sqSector, len(sectors))
	for i, s := range sectors {
		items[i] = sqSector{sector: s, area: sectorArea(s) / total * area}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].sector.Order != items[j].sector.Order {
			return items[i].sector.Order < items[j].sector.Order
		}
		return items[i].area > items[j].area
	})
	squarifySectors(items, 0, 0, w, h)
}

type sqSector struct {
	sector *mapSector
	area   float64
}

func squarifySectors(items []sqSector, x, y, w, h float64) {
	if len(items) == 0 {
		return
	}
	if len(items) == 1 {
		items[0].sector.X = x
		items[0].sector.Y = y
		items[0].sector.W = w
		items[0].sector.H = h
		return
	}
	row := []sqSector{}
	rest := items
	bestRatio := math.Inf(1)
	rowSum := 0.0
	for len(rest) > 0 {
		newRow := append([]sqSector{}, row...)
		newRow = append(newRow, rest[0])
		newRowSum := rowSum + rest[0].area
		ratio := worstRatioSector(newRow, newRowSum, w, h)
		if ratio < bestRatio {
			bestRatio = ratio
			row = newRow
			rowSum = newRowSum
			rest = rest[1:]
		} else {
			break
		}
	}
	if len(row) == 0 {
		row = []sqSector{rest[0]}
		rowSum = rest[0].area
		rest = rest[1:]
	}
	if w >= h {
		rowH := rowSum / w
		cx := x
		for _, it := range row {
			iw := it.area / rowH
			it.sector.X = cx
			it.sector.Y = y
			it.sector.W = iw
			it.sector.H = rowH
			cx += iw
		}
		squarifySectors(rest, x, y+rowH, w, h-rowH)
	} else {
		rowW := rowSum / h
		cy := y
		for _, it := range row {
			ih := it.area / rowW
			it.sector.X = x
			it.sector.Y = cy
			it.sector.W = rowW
			it.sector.H = ih
			cy += ih
		}
		squarifySectors(rest, x+rowW, y, w-rowW, h)
	}
}

func worstRatioSector(row []sqSector, rowSum, w, h float64) float64 {
	var worst float64
	if w >= h {
		rowH := rowSum / w
		for _, it := range row {
			iw := it.area / rowH
			r := iw / rowH
			if r < 1 {
				r = 1 / r
			}
			if r > worst {
				worst = r
			}
		}
	} else {
		rowW := rowSum / h
		for _, it := range row {
			ih := it.area / rowW
			r := rowW / ih
			if r < 1 {
				r = 1 / r
			}
			if r > worst {
				worst = r
			}
		}
	}
	return worst
}

// layoutTiles runs a squarified treemap over tiles within a sub-rectangle.
// Tile area comes from the severity/duration weight so failures read large.
func layoutTiles(tiles []*mapTile, x, y, w, h float64) {
	if len(tiles) == 0 {
		return
	}
	total := 0.0
	for _, t := range tiles {
		total += tileArea(t)
	}
	if total <= 0 {
		return
	}
	area := w * h
	items := make([]sqItem, len(tiles))
	for i, t := range tiles {
		items[i] = sqItem{tile: t, area: tileArea(t) / total * area}
	}
	// Largest (most urgent) first, then alphabetical for a stable layout.
	sort.Slice(items, func(i, j int) bool {
		if items[i].area != items[j].area {
			return items[i].area > items[j].area
		}
		return items[i].tile.Label < items[j].tile.Label
	})
	squarifyItems(items, x, y, w, h)
}

func squarifyItems(items []sqItem, x, y, w, h float64) {
	if len(items) == 0 {
		return
	}
	if len(items) == 1 {
		items[0].tile.X = x
		items[0].tile.Y = y
		items[0].tile.W = w
		items[0].tile.H = h
		return
	}
	row := []sqItem{}
	rest := items
	bestRatio := math.Inf(1)
	rowSum := 0.0
	for len(rest) > 0 {
		newRow := append([]sqItem{}, row...)
		newRow = append(newRow, rest[0])
		newRowSum := rowSum + rest[0].area
		ratio := worstRatio(newRow, newRowSum, w, h)
		if ratio < bestRatio {
			bestRatio = ratio
			row = newRow
			rowSum = newRowSum
			rest = rest[1:]
		} else {
			break
		}
	}
	if len(row) == 0 {
		row = []sqItem{rest[0]}
		rowSum = rest[0].area
		rest = rest[1:]
	}
	if w >= h {
		rowH := rowSum / w
		cx := x
		for _, it := range row {
			iw := it.area / rowH
			it.tile.X = cx
			it.tile.Y = y
			it.tile.W = iw
			it.tile.H = rowH
			cx += iw
		}
		squarifyItems(rest, x, y+rowH, w, h-rowH)
	} else {
		rowW := rowSum / h
		cy := y
		for _, it := range row {
			ih := it.area / rowW
			it.tile.X = x
			it.tile.Y = cy
			it.tile.W = rowW
			it.tile.H = ih
			cy += ih
		}
		squarifyItems(rest, x+rowW, y, w-rowW, h)
	}
}

func worstRatio(row []sqItem, rowSum, w, h float64) float64 {
	var worst float64
	if w >= h {
		rowH := rowSum / w
		for _, it := range row {
			iw := it.area / rowH
			r := iw / rowH
			if r < 1 {
				r = 1 / r
			}
			if r > worst {
				worst = r
			}
		}
	} else {
		rowW := rowSum / h
		for _, it := range row {
			ih := it.area / rowW
			r := rowW / ih
			if r < 1 {
				r = 1 / r
			}
			if r > worst {
				worst = r
			}
		}
	}
	return worst
}
