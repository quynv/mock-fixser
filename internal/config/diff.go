package config

import "reflect"

// Diff summarizes what changed between two configs (design doc, section 9.3).
type Diff struct {
	// Sections lists the changed parts, e.g. "symbols" or "server.fix.sessions".
	Sections []string `json:"sections"`
	// RestartRequired lists changed fields that only take effect after a restart.
	RestartRequired []string     `json:"restart_required,omitempty"`
	SessionsAdded   []SessionKey `json:"sessions_added,omitempty"`
	SessionsRemoved []SessionKey `json:"sessions_removed,omitempty"`
}

// Empty reports whether nothing but the version changed.
func (d Diff) Empty() bool { return len(d.Sections) == 0 }

// Compare returns the differences from old to new, ignoring Version.
func Compare(old, new *Config) Diff {
	var d Diff
	check := func(name string, a, b any, restart bool) {
		if reflect.DeepEqual(a, b) {
			return
		}
		d.Sections = append(d.Sections, name)
		if restart {
			d.RestartRequired = append(d.RestartRequired, name)
		}
	}

	of, nf := old.Server.FIX, new.Server.FIX
	check("server.fix.listen", of.Listen, nf.Listen, true)
	check("server.fix.version", of.Version, nf.Version, true)
	check("server.fix.data_dictionary", of.DataDictionary, nf.DataDictionary, true)
	check("server.fix.tls", of.TLS, nf.TLS, true)
	check("server.fix.sessions", of.Sessions, nf.Sessions, false)

	om, nm := old.Server.Monitor, new.Server.Monitor
	check("server.monitor.enabled", om.Enabled, nm.Enabled, true)
	check("server.monitor.listen", om.Listen, nm.Listen, true)
	check("server.monitor.comp_ids",
		[2]string{om.SenderCompID, om.TargetCompID}, [2]string{nm.SenderCompID, nm.TargetCompID}, true)
	om.Enabled, om.Listen, om.SenderCompID, om.TargetCompID = nm.Enabled, nm.Listen, nm.SenderCompID, nm.TargetCompID
	check("server.monitor", om, nm, false)

	check("server.http.listen", old.Server.HTTP.Listen, new.Server.HTTP.Listen, true)
	check("server.http.history_ticks", old.Server.HTTP.HistoryTicks, new.Server.HTTP.HistoryTicks, false)

	check("market_data", old.MarketData, new.MarketData, false)
	check("symbols", old.Symbols, new.Symbols, false)
	check("order_rules", old.OrderRules, new.OrderRules, false)
	check("default_action", old.DefaultAction, new.DefaultAction, false)
	check("order_lists", old.OrderLists, new.OrderLists, false)
	check("list_rules", old.ListRules, new.ListRules, false)

	oldVersions := map[SessionKey]string{}
	for _, s := range of.Sessions {
		oldVersions[s.Key()] = old.SessionVersion(s)
	}
	newKeys := map[SessionKey]bool{}
	for _, s := range nf.Sessions {
		newKeys[s.Key()] = true
		v := new.SessionVersion(s)
		ov, existed := oldVersions[s.Key()]
		if !existed {
			d.SessionsAdded = append(d.SessionsAdded, s.Key())
		}
		// Sessions of other versions than server.fix.version are set up when
		// the acceptor starts (section 5.3).
		if (!existed && v != nf.Version) || (existed && ov != v) {
			d.RestartRequired = append(d.RestartRequired, "server.fix.sessions "+s.Key().String()+" "+v)
		}
	}
	for _, s := range of.Sessions {
		if !newKeys[s.Key()] {
			d.SessionsRemoved = append(d.SessionsRemoved, s.Key())
		}
	}
	return d
}
