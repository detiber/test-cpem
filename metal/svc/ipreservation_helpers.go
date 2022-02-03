package svc

import (
	"github.com/packethost/packngo"
)

// IPReservationByAllTags given a set of packngo.IPAddressReservation and a set of tags, find
// the first reservation that has all of those tags
func IPReservationByAllTags(targetTags []string, ips []packngo.IPAddressReservation) *packngo.IPAddressReservation {
	ret := IPReservationsByAllTags(targetTags, ips)
	if len(ret) > 0 {
		return ret[0]
	}
	return nil
}

// IPReservationsByAllTags given a set of packngo.IPAddressReservation and a set of tags, find
// all of the reservations that have all of those tags
func IPReservationsByAllTags(targetTags []string, ips []packngo.IPAddressReservation) []*packngo.IPAddressReservation {
	// cycle through the IPs, looking for one that matches ours
	ret := []*packngo.IPAddressReservation{}
ips:
	for i, ip := range ips {
		tagMatches := map[string]bool{}
		for _, t := range targetTags {
			tagMatches[t] = false
		}
		for _, tag := range ip.Tags {
			if _, ok := tagMatches[tag]; ok {
				tagMatches[tag] = true
			}
		}
		// does this IP match?
		for _, v := range tagMatches {
			// any missing tag says no match
			if !v {
				continue ips
			}
		}
		// if we made it here, we matched
		ret = append(ret, &ips[i])
	}
	return ret
}
