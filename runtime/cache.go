/**
 * Tenta DNS Server
 *
 *    Copyright 2017 Tenta, LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * For any questions, please contact developer@tenta.io
 *
 * cache.go: Cache for dns resolver
 */

package runtime

import (
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

const (
	CACHE_EVICTION_RATE = 30 /// expressed in seconds
	CACHE_OPENNIC       = "opennic"
	CACHE_IANA          = "iana"
)

const (
	KV_TLS_CAPABILITY = "tlscap"
	KV_TCP_PREFERENCE = "tcppref"
	KV_EDNS_ALLERGY   = "skipedns"
	KV_DS_RR_NUM      = "ds_rr_num"
	KV_DNSKEY_RR_NUM  = "dnskey_rr_num"
)

const (
	ITEM_CACHE_DNSSEC_DESIGNATION = "dnssec-"
)

type DNSCacheHolder struct {
	m map[string]*DNSCache /// multiplexer for multiple insulated caches
}

type DNSCache struct {
	m  *sync.RWMutex           /// global read-write mutex; write is used for map-level operations (INS/DEL keys, cleanup)
	c  *cleanup                /// global cleanup
	l  map[string]*domainCache /// the effective front-facing layer of the cache
	k  *sync.Map               /// key-value store attached to every instance of cache (storing non-RR data)
	lg *logrus.Entry           /// logging
}

type domainCache struct {
	m *sync.RWMutex                         /// ops mutex
	l map[uint16]map[string]opaqueCacheItem /// an RR list/map, in all its splendor
	/// rationale behind using a map (vs list/array) is as follows:
}

type opaqueCacheItem interface {
	isDNSSECStore() bool
	mapKey() string
	keyQType() uint16
	timeCreated() time.Time
	validity() time.Duration
	adjustValidity(int64)
}

type responseCache struct {
	time.Time
	time.Duration
	*dns.Msg
}

type itemCache struct {
	time.Time                     /// time created
	time.Duration                 /// ttl value
	dns.RR                        /// the actual record
	val           *ItemCacheExtra /// other values stored pertaining to the record (DNSSEC situation, etc)
}

type cleanup struct {
	i map[int64][]*cleanupItem /// list of cleanable items (key is unix timestamp)
	t *time.Ticker             /// cleanup interval ticker
	c chan *cleanupItem        /// chan to receive cleanup targets
	q chan bool                /// stop chan
	w *sync.WaitGroup          /// stop wait sync
	o int64                    /// origin -- works as a clock skew equalizer -- map keys are synchronized to this value (o + n*INTERVAL);
	/// and at every interval current time is also normalized to this value -- it's updated every cleanup cycle
}

type cleanupItem struct {
	firstKey  string /// key in DNSCache map
	secondKey uint16 /// key in domainCache first map, aka map[uin16]map[string]dns.RR
	///									   					   ^^^^^ this one
	key string /// key in domainCache second map, aka map[uin16]map[string]dns.RR
	///									   						    ^^^^^^ this one
	when int64 /// unix timestamp of when the item is planned for eviction
}

type ItemCacheExtra struct {
	Nxdomain, Nodata, Cname bool
	Redirect                []*dns.CNAME
}

/*
** Opaque Cache Item implementations
 */

func (i *itemCache) isDNSSECStore() bool {
	return false
}

func (i *itemCache) mapKey() string {
	return neutralizeRecord(i.RR)
}

func (i *itemCache) keyQType() uint16 {
	return i.RR.Header().Rrtype
}

func (i *itemCache) timeCreated() time.Time {
	return i.Time
}

func (i *itemCache) validity() time.Duration {
	return i.Duration
}

func (i *itemCache) adjustValidity(delta int64) {
	i.Header().Ttl = uint32(int64(i.Header().Ttl) + delta)
	i.Duration = time.Duration(i.Header().Ttl) * time.Second
}

func (r *responseCache) isDNSSECStore() bool {
	return true
}

func (r *responseCache) mapKey() string {
	return ITEM_CACHE_DNSSEC_DESIGNATION + dns.TypeToString[r.Question[0].Qtype]
}

func (r *responseCache) keyQType() uint16 {
	return r.Question[0].Qtype
}

func (r *responseCache) timeCreated() time.Time {
	return r.Time
}

func (r *responseCache) validity() time.Duration {
	return r.Duration
}

func (r *responseCache) adjustValidity(delta int64) {
	rrHolder := [][]dns.RR{r.Answer, r.Ns, r.Extra}
	minTTL := uint32(72 * time.Hour / time.Second)
	for _, h := range rrHolder {
		for _, rr := range h {
			rr.Header().Ttl = uint32(int64(rr.Header().Ttl) + delta)
			if rr.Header().Ttl < minTTL {
				minTTL = rr.Header().Ttl
			}
		}
	}
	r.Duration = time.Duration(minTTL) * time.Second
}

/*
** Runtime module functions
 */

// StartCache -- Creates, starts and returns a cache object
func StartCache(log *logrus.Entry, designations ...string) *DNSCacheHolder {
	ret := &DNSCacheHolder{make(map[string]*DNSCache)}

	for _, cn := range designations {
		ret.m[cn] = &DNSCache{
			m:  new(sync.RWMutex),
			c:  newCleanup(),
			l:  make(map[string]*domainCache),
			k:  new(sync.Map),
			lg: log.WithField("provider", cn),
		}
	}

	if len(designations) != len(ret.m) {
		log.Fatalf("Supplied %d names out of which only %d is unique.", len(designations), len(ret.m))
		panic("Cannot start cache with ambiguous names")
	}

	for _, c := range ret.m {
		c.startCleanup()
	}

	return ret
}

// Stop -- stops caching (stops cleanup thread)
func (d *DNSCacheHolder) Stop() {
	for _, c := range d.m {
		c.stopCleanup()
	}
}

/*
** KV Store primitives
 */

func (d *DNSCacheHolder) Put(provider, key string, value interface{}) {
	d.m[provider].k.Store(key, value)
}

func (d *DNSCacheHolder) Get(provider, key string) interface{} {
	ret, _ := d.m[provider].k.Load(key)
	return ret
}

func (d *DNSCacheHolder) GetString(provider, key string) (string, bool) {
	ret, ok := d.m[provider].k.Load(key)
	if !ok {
		return "", false
	}
	rets, ok := ret.(string)
	if !ok {
		return "", false
	}
	return rets, true
}

func (d *DNSCacheHolder) GetInt(provider, key string) (int, bool) {
	ret, ok := d.m[provider].k.Load(key)
	if !ok {
		return 0, false
	}
	reti, ok := ret.(int)
	if !ok {
		return 0, false
	}
	return reti, true
}

func (d *DNSCacheHolder) GetBool(provider, key string) (bool, bool) {
	ret, ok := d.m[provider].k.Load(key)
	if !ok {
		return false, false
	}
	retb, ok := ret.(bool)
	if !ok {
		return false, false
	}
	return retb, true
}

/*
** Core cache functionalities
 */

func (d *DNSCacheHolder) Insert(provider, domain string, rr dns.RR, extra *ItemCacheExtra) {
	/// drop entries with 0 TTL
	if rr.Header().Ttl == 0 {
		return
	}
	/// concurrent read from a generic map
	if c, ok := d.m[provider]; ok {
		c.insert(domain, rr, extra)
	}
}

func (d *DNSCacheHolder) InsertResponse(provider, domain string, r *dns.Msg) {
	/// it's oversimplified, but needs to be in order to be in sync with the rest of the cache
	/// TODO: complicate this part a bit
	for _, h := range [][]dns.RR{r.Answer, r.Ns, r.Extra} {
		for _, rr := range h {
			if rr.Header().Ttl == 0 {
				return
			}
		}
	}
	if c, ok := d.m[provider]; ok {
		c.insertResponse(domain, r)
	}
}

func (d *DNSCacheHolder) Retrieve(provider, domain string, t uint16, dnssec bool) (ret interface{}, extra *ItemCacheExtra) {
	if c, ok := d.m[provider]; ok {
		return c.retrieve(domain, t, dnssec)
	}
	return nil, nil
}

func itemCacheFromRR(rr dns.RR, extra *ItemCacheExtra) *itemCache {
	return &itemCache{time.Now(), time.Duration(rr.Header().Ttl) * time.Second, rr, extra}
}

func responseCacheFromMsg(m *dns.Msg) *responseCache {
	minTTL := time.Hour * 72
	rrHolder := [][]dns.RR{m.Answer, m.Ns, m.Extra}

	for _, h := range rrHolder {
		for _, rr := range h {
			if minTTL > time.Duration(rr.Header().Ttl)*time.Second {
				minTTL = time.Duration(rr.Header().Ttl) * time.Second
			}
		}
	}

	return &responseCache{time.Now(), minTTL, m}
}

/// returns a string reprezentation of a resource record, with volatile parts wiped (eg. TTL) for comparison purposes
func neutralizeRecord(rr dns.RR) string {
	t := dns.Copy(rr)
	t.Header().Ttl = 0

	switch rec := t.(type) {
	case *dns.SOA:
		rec.Expire, rec.Minttl, rec.Refresh, rec.Retry, rec.Serial = 0, 0, 0, 0, 0
	}
	return t.String()
}

func (d *DNSCache) insert(domain string, rr dns.RR, extra *ItemCacheExtra) {
	d.insertInternal(domain, itemCacheFromRR(rr, extra))
}

func (d *DNSCache) insertResponse(domain string, resp *dns.Msg) {
	d.insertInternal(domain, responseCacheFromMsg(resp))
}

func (d *DNSCache) insertInternal(_domain string, cachee opaqueCacheItem) {
	domain := strings.ToLower(_domain)
	// d.lg.Infof("Inserting for [%s]", domain)
	d.m.Lock()
	dom, ok := d.l[domain]
	if !ok {
		dom = &domainCache{new(sync.RWMutex), make(map[uint16]map[string]opaqueCacheItem)}
		d.l[domain] = dom
	}
	dom.m.Lock()
	defer dom.m.Unlock()
	d.m.Unlock()
	rrtype := cachee.keyQType()
	if _, ok := dom.l[rrtype]; !ok {
		dom.l[rrtype] = make(map[string]opaqueCacheItem)
	}
	dom.l[rrtype][cachee.mapKey()] = cachee
	/// submit item for cleanup
	// d.c.c <- &cleanupItem{
	// 	domain, rrtype, cachee.mapKey(),
	// 	time.Now().Unix() +
	// 		int64(cachee.validity()/time.Second)}
}

func (d *DNSCache) retrieve(domain string, t uint16, dnssec bool) (ret interface{}, extra *ItemCacheExtra) {
	// d.lg.Infof("Retrieving for [%s/%s/%v]", domain, dns.TypeToString[t], dnssec)
	d.m.RLock()
	dom, ok := d.l[domain]
	if !ok {
		d.m.RUnlock()
		return
	}
	dom.m.RLock()
	d.m.RUnlock()
	retRegular := []dns.RR{}
	interm := dom.l[t]
	// d.lg.Debugf("Iterating store with size [%d]", len(interm))
	for k, v := range interm {
		// switch cahceElemType := v.(type) {
		// case *itemCache:
		// 	d.lg.Debugf("We have regular [%s] -- [%v]", k, v)
		// case *responseCache:
		// 	d.lg.Debugf("We have DNSSEC  [%s] -- [%s]", k, cahceElemType.Question[0].String())
		// }

		/// if item is queried before rounded eviction time
		if v.timeCreated().Add(v.validity()).Before(time.Now()) {
			// d.lg.Debugf("Deleting record, because [%v] + [%v] > [%v]", v.timeCreated(), v.validity(), time.Now())
			defer func() {
				dom.m.Lock()
				delete(interm, k)
				dom.m.Unlock()
			}()
			continue
		} else { /// if opaque cache item has valid TTL
			// d.lg.Debugf("Item is within validity period. Returning as requested, or as possible.")
			if dnssec && v.isDNSSECStore() { /// if we need dnssec and we have a dnssec response, we return *the* response (only one of those per RRtype)
				// d.lg.Debugf("Returning DNSSEC cache -- [%v]", v.(*responseCache).Msg.Question[0])
				// defer func(m *sync.RWMutex) {
				// 	m.Lock()
				// 	v.adjustValidity(int64(-time.Now().Sub(v.timeCreated()) / time.Second))
				// 	m.Unlock()
				// }(dom.m)
				src := v.(*responseCache).Msg
				retResp := cloneResponse(src)
				for hldIndex, holder := range [][]dns.RR{src.Answer, src.Ns, cleanAdditionalSection(src.Extra)} {
					for _, rr := range holder {
						if rr != nil {
							rr.Header().Ttl = rr.Header().Ttl - uint32(time.Now().Sub(v.timeCreated())/time.Second)
						} else if hldIndex == 0 { /// if a record got stale from the answer section, remove this entry from cache
							defer func() {
								dom.m.Lock()
								delete(interm, k)
								dom.m.Unlock()
							}()
							continue
						}
					}
				}
				dom.m.RUnlock()
				return retResp, nil
			} else if !v.isDNSSECStore() {
				// d.lg.Debugf("Returning regular cache item -- [%v]", v.(*itemCache).RR)
				// defer func(m *sync.RWMutex) {
				// 	m.Lock()
				// v.adjustValidity(int64(-time.Now().Sub(v.timeCreated()) / time.Second))
				// 	m.Unlock()
				// }(dom.m)
				retRR := dns.Copy(v.(*itemCache).RR)
				if retRR.Header().Ttl > uint32(time.Now().Sub(v.timeCreated())/time.Second) {
					retRR.Header().Ttl = retRR.Header().Ttl - uint32(time.Now().Sub(v.timeCreated())/time.Second)
				} else {
					defer func() {
						dom.m.Lock()
						delete(interm, k)
						dom.m.Unlock()
					}()
					continue
				}
				retRegular = append(retRegular, retRR)
				if extra == nil && v.(*itemCache).val != nil {
					extra = v.(*itemCache).val
				}
			}
		}
	}
	if !dnssec || (dnssec && len(retRegular) > 0) {
		dom.m.RUnlock()
		return retRegular, extra
	}
	/// return a nil struct pointer so the interface (ptr) itself wouldn't be nil
	var retDummyDnssec *dns.Msg
	dom.m.RUnlock()
	return retDummyDnssec, nil
}

func cleanAdditionalSection(extra []dns.RR) (clean []dns.RR) {
	for _, rr := range extra {
		if rr != nil && rr.Header().Rrtype != dns.TypeOPT {
			clean = append(clean, rr)
		}
	}
	return
}

func cloneSection(s []dns.RR) (out []dns.RR) {
	for _, rr := range s {
		if rr != nil {
			out = append(out, dns.Copy(rr))
		}
	}
	return
}

func cloneResponse(in *dns.Msg) (out *dns.Msg) {
	out = &dns.Msg{MsgHdr: in.MsgHdr}
	out.Question = make([]dns.Question, len(in.Question))
	copy(in.Question, out.Question)
	out.Answer = cloneSection(in.Answer)
	out.Ns = cloneSection(in.Ns)
	out.Extra = cloneSection(in.Extra)
	return
}

/*
** Cache cleanup
 */

func newCleanup() *cleanup {
	return &cleanup{make(map[int64][]*cleanupItem), time.NewTicker(CACHE_EVICTION_RATE * time.Second), make(chan *cleanupItem, 1000),
		make(chan bool, 1), new(sync.WaitGroup), time.Now().Unix()}
}

func (d *DNSCache) startCleanup() {
	d.c.w.Add(1)
	go func() {
		defer d.c.w.Done()
		isQuitting := false
		for {
			select {
			/// time for cleanup
			// case <-d.c.t.C:
			// 	/// update origin
			// 	d.c.o += CACHE_EVICTION_RATE
			// 	/// get cleanable elements
			// 	evictees := d.c.i[d.c.o]
			// 	/// cycle all elements and remove references to them
			// 	cleanStart := time.Now()
			// 	timeWait := time.Duration(0)
			// 	for _, e := range evictees {

			// 		fmt.Printf("Evicting [%s/%s/%s]\n", e.firstKey, dns.TypeToString[e.secondKey], e.key)
			// 		yolo := time.Now()
			// 		d.m.RLock()
			// 		timeWait += time.Now().Sub(yolo)
			// 		dom, ok := d.l[e.firstKey]
			// 		if !ok {
			// 			d.m.RUnlock()
			// 			/// this should raise some eyebrows
			// 			continue
			// 		}
			// 		yolo = time.Now()
			// 		dom.m.Lock()
			// 		timeWait += time.Now().Sub(yolo)
			// 		d.m.RUnlock()
			// 		/// we delete the key
			// 		delete(dom.l[e.secondKey], e.key)
			// 		/// if we left the type map empty, delete the type index too
			// 		if len(dom.l[e.secondKey]) == 0 {
			// 			delete(dom.l, e.secondKey)
			// 		}
			// 		dom.m.Unlock()
			// 	}
			// 	d.lg.Infof("Evicted [%d] items, in %v time out of which %v was lockwait", len(evictees), time.Now().Sub(cleanStart), timeWait)
			case <-d.c.q:
				/// maybe a simple return would suffice here?
				isQuitting = true
				break
				// case target := <-d.c.c:
				// 	if target.when < d.c.o+CACHE_EVICTION_RATE {
				// 		continue
				// 	}
				// 	index := d.c.o + int64(math.Floor(float64(target.when-d.c.o)/float64(CACHE_EVICTION_RATE)))
				// 	d.c.i[index] = append(d.c.i[index], target)
			}
			if isQuitting == true {
				break
			}
		}
	}()

}

func (d *DNSCache) stopCleanup() {
	d.c.q <- true
	d.lg.Debugf("Sent stop signal to cache cleaner")
	d.c.w.Wait()
}

/*
** Helpers and convenience methods
 */
// MapKey -- creates a key-value store key with the given prefix and suffix. (to put simply joins them with a colon char)
func MapKey(prefix, suffix string) string {
	return prefix + ":" + suffix
}

func AsRR(in interface{}) []dns.RR {
	if ret, ok := in.([]dns.RR); ok {
		return ret
	}
	return nil
}

func AsMsg(in interface{}) *dns.Msg {
	if ret, ok := in.(*dns.Msg); ok {
		return ret
	}
	return nil
}
