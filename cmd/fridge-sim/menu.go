package main

import "math/rand"

// menuItem is one item a fridge slot can be stocked with. Names and
// categories are real, pulled from Farmer's Fridge's public menu page
// (farmersfridge.com/menu, captured 2026-08-25) -- not invented. Farmer's
// Fridge does not publish per-item pricing; PriceCentsMin/Max is a
// plausible reconstruction within the overall range a third-party
// menu-price tracker reports for their lineup (roughly $2-$8), split by
// category (snacks/sides lower, salads/bowls/wraps/breakfast/high-protein
// higher). This is explicitly NOT their actual current pricing.
type menuItem struct {
	Name          string
	Category      string
	PriceCentsMin int
	PriceCentsMax int
}

var menu = []menuItem{
	{"Steakhouse Chopped Salad", "Salad", 600, 800},
	{"Sweet Street Toffee Crunch Blondie", "Snack", 200, 400},
	{"Huevos Rancheros Breakfast Bowl", "Breakfast", 600, 800},
	{"Shawarma Spiced Chickpea Wrap", "Wrap", 600, 800},
	{"High Protein Grilled Chicken & Veggie Bowl", "HighProtein", 600, 800},
	{"Caprese Salad", "Salad", 600, 800},
	{"Greek Salad", "Salad", 600, 800},
	{"High Protein Chicken Shawarma Bowl", "HighProtein", 600, 800},
	{"Strawberries & Cream Chia Pudding", "Breakfast", 600, 800},
	{"Chile Lime Caesar Salad", "Salad", 600, 800},
}

func randomMenuItem(rng *rand.Rand) menuItem {
	return menu[rng.Intn(len(menu))]
}

// randomPriceCents picks one price within the item's category band. Called
// once per slot at fridge-creation time -- a slot's price stays fixed for
// every vend of that slot afterward, same as a real menu.
func (m menuItem) randomPriceCents(rng *rand.Rand) int {
	return m.PriceCentsMin + rng.Intn(m.PriceCentsMax-m.PriceCentsMin+1)
}
