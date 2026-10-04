//go:build ignore

package main

import("fmt";"os";dem "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs";"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events";"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common")
func main(){f,_:=os.Open("/home/water/Project Autocs2video/9206662465244206092_0.dem");defer f.Close();p:=dem.NewParser(f);defer p.Close();p.RegisterEventHandler(func(e events.InfernoExpired){if e.Inferno.Thrower()!=nil{fmt.Println("EXPIRE",p.GameState().IngameTick(),e.Inferno.Entity.ID(),e.Inferno.Thrower().SteamID64,e.Inferno.Entity.Position())}});p.RegisterEventHandler(func(e events.Kill){if e.Weapon!=nil&&e.Weapon.Type==common.EqMolotov{fmt.Println("DEATH",p.GameState().IngameTick(),e.Killer.SteamID64,e.Victim.Position(),e.Weapon.OriginalString);for _,inf:=range p.GameState().Infernos(){owner:=uint64(0);if inf.Thrower()!=nil{owner=inf.Thrower().SteamID64};fmt.Println("FIRE",inf.Entity.ID(),owner,inf.Entity.Position(),inf.Fires().Active().List(),"ALL",inf.Fires().List())}}});if e:=p.ParseToEnd();e!=nil{panic(e)}}
