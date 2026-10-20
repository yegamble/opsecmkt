package market

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
)

type CatalogCategory struct {
	Name          string
	Subcategories []string
}

var catalogCategories = []CatalogCategory{
	{"Hardware", []string{"USB / Storage", "RF / RFID", "Faraday / Shielding", "Network", "Surveillance"}},
	{"Digital", []string{"Research / Reports", "Guides / Courses", "Software / Tools", "Credentials / Access"}},
	{"Services", []string{"Hosting / VPS", "Consulting", "Custom Dev"}},
}

var shippingRegions = []string{"Worldwide", "North America", "United States", "Europe", "Asia", "Middle East", "Africa", "Oceania", "South America"}

func (d PageData) CatalogCategories() []CatalogCategory { return catalogCategories }
func (d PageData) ShippingRegions() []string            { return shippingRegions }

func validSubcategory(category, subcategory string) bool {
	if subcategory == "" {
		return true
	}
	for _, c := range catalogCategories {
		if (category == "" || c.Name == category) && slices.Contains(c.Subcategories, subcategory) {
			return true
		}
	}
	return false
}

func (d PageData) catalogValues() url.Values {
	v := url.Values{}
	if d.InStockOnly {
		v.Set("in_stock", "1")
	}
	for k, x := range map[string]string{"q": d.Query, "category": d.Category, "subcategory": d.Subcategory, "region": d.Region, "kind": d.KindFilter, "sort": d.Sort} {
		if x != "" && !(k == "sort" && x == "newest") {
			v.Set(k, x)
		}
	}
	return v
}

// CatalogCategoryURL changes just the taxonomy; search, fulfillment and shipping filters remain selected.
func (d PageData) CatalogCategoryURL(category, subcategory string) string {
	v := d.catalogValues()
	v.Del("category")
	v.Del("subcategory")
	if category != "" {
		v.Set("category", category)
	}
	if subcategory != "" {
		v.Set("subcategory", subcategory)
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

func (p Product) AdditionalShipping(region string) bool {
	return slices.Contains(p.ShippingRegions, region)
}
func (p Product) KnownRegion() bool {
	return p.Region == "Digital" || slices.Contains(shippingRegions, p.Region)
}

func (p Product) ShippingLabel() string {
	if p.Region == "Worldwide" || slices.Contains(p.ShippingRegions, "Worldwide") {
		return "Worldwide"
	}
	regions := []string{p.Region}
	for _, r := range p.ShippingRegions {
		if !slices.Contains(regions, r) {
			regions = append(regions, r)
		}
	}
	return strings.Join(regions, ", ")
}

func (p Product) shipsTo(region string) bool {
	if region == "" {
		return true
	}
	if p.Region == "Worldwide" || slices.Contains(p.ShippingRegions, "Worldwide") {
		return true
	}
	if p.Region == region || slices.Contains(p.ShippingRegions, region) {
		return true
	}
	return (region == "North America" && (p.Region == "United States" || slices.Contains(p.ShippingRegions, "United States"))) ||
		(region == "United States" && (p.Region == "North America" || slices.Contains(p.ShippingRegions, "North America")))
}

func (p Product) Rating() string { return fmt.Sprintf("%.1f", p.RatingAverage) }

func (d *PageData) sortPreviewProducts() {
	sort.SliceStable(d.Products, func(i, j int) bool {
		a, b := d.Products[i], d.Products[j]
		switch d.Sort {
		case "price-low", "price-high":
			x, _ := parseAmount(a.PriceXMR, 12)
			y, _ := parseAmount(b.PriceXMR, 12)
			if d.Sort == "price-high" {
				return x > y
			}
			return x < y
		case "rating":
			if a.RatingAverage == b.RatingAverage {
				return a.RatingCount > b.RatingCount
			}
			return a.RatingAverage > b.RatingAverage
		}
		return false
	})
}
