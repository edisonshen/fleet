package main
import (
  "fmt"
  "github.com/edisonshen/fleet/internal/state"
)
func main(){ fmt.Printf("%q\n", state.SafeLockComponent(".")); fmt.Printf("%q\n", state.SafeLockComponent("..")) }
