// Command tapio is Tapioca under a shorter name. Shopify's tapioca gem
// installs a binary called `tapioca` too, and whichever is later on PATH wins,
// so the same program is shipped under both names and neither has to lose.
package main

import "tapioca/internal/cli"

func main() { cli.Main() }
