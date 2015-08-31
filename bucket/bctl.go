package bucket

import (
	"github.com/DemonVex/backrunner/config"
	"github.com/DemonVex/backrunner/errors"
	"github.com/DemonVex/backrunner/etransport"
	"github.com/DemonVex/backrunner/reply"
	"github.com/bioothod/elliptics-go/elliptics"
	"fmt"
	"io/ioutil"
	"log"
	//"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	ProfilePath string = "backrunner.profile"

	PainNoFreeSpaceSoft float64	= 5000000000.0
	PainNoFreeSpaceHard float64	= 50000000000000.0

	// pain for read-only groups
	PainStatRO float64		= PainNoFreeSpaceHard / 2

	// this is randomly selected error gain for buckets where upload has failed
	BucketWriteErrorPain float64	= PainNoFreeSpaceHard / 2

	// pain for group without statistics
	PainNoStats float64		= PainNoFreeSpaceHard / 2

	// pain for group where statistics contains error field
	PainStatError float64		= PainNoFreeSpaceHard / 2

	// pain for bucket which do not have its group in stats
	PainNoGroup float64		= PainNoFreeSpaceHard / 2

	PainDiscrepancy float64		= 1000.0
)

func URIOffsetSize(req *http.Request) (offset uint64, size uint64, err error) {
	offset = 0
	size = 0

	q := req.URL.Query()
	offset_str := q.Get("offset")
	if offset_str != "" {
		offset, err = strconv.ParseUint(offset_str, 0, 64)
		if err != nil {
			err = fmt.Errorf("could not parse offset URI: %s: %v", offset_str, err)
			return
		}
	}

	size_str := q.Get("size")
	if size_str != "" {
		size, err = strconv.ParseUint(size_str, 0, 64)
		if err != nil {
			err = fmt.Errorf("could not parse size URI: %s: %v", size_str, err)
			return
		}
	}

	return offset, size, nil
}

type BucketCtl struct {
	sync.RWMutex

	bucket_path		string
	e			*etransport.Elliptics

	proxy_config_path	string
	Conf			*config.ProxyConfig

	signals			chan os.Signal

	BucketTimer		*time.Timer
	BucketStatTimer		*time.Timer

	StatTime		time.Time

	// time when previous defragmentation scan was performed
	DefragTime		time.Time

	// buckets used for automatic write bucket selection,
	// i.e. when client doesn't provide bucket name and we select it
	// according to its performance and capacity
	Bucket			[]*Bucket

	// buckets used by clients directly, i.e. when client explicitly says
	// he wants to work with bucket named 'X'
	BackBucket		[]*Bucket
}

func (bctl *BucketCtl) AllBuckets() []*Bucket {
	out := bctl.Bucket
	return append(out, bctl.BackBucket...)
}

func (bctl *BucketCtl) FindBucketRO(name string) *Bucket {
	bctl.RLock()
	defer bctl.RUnlock()

	for _, b := range bctl.AllBuckets() {
		if b.Name == name {
			return b
		}
	}

	return nil
}

func (bctl *BucketCtl) FindBucket(name string) (bucket *Bucket, err error) {
	bucket = bctl.FindBucketRO(name)
	if bucket == nil {
		b, err := ReadBucket(bctl.e, name)
		if err != nil {
			return nil, fmt.Errorf("%s: could not find and read bucket: %v", name, err.Error())
		}

		bctl.Lock()
		bctl.BackBucket = append(bctl.BackBucket, b)
		bucket = b
		bctl.Unlock()
	}

	return bucket, nil
}

func (bctl *BucketCtl) BucketStatUpdateNolock(stat *elliptics.DnetStat) (err error) {
	bctl.StatTime = stat.Time

	for _, b := range bctl.AllBuckets() {
		for _, group := range b.Meta.Groups {
			sg, ok := stat.Group[group]
			if ok {
				b.Group[group] = sg
			} else {
				log.Printf("bucket-stat-update: bucket: %s, group: %d: there is no bucket stat, using old values (if any)",
					b.Name, group)
			}
		}
	}

	return
}

func (bctl *BucketCtl) BucketStatUpdate() (err error) {
	stat, err := bctl.e.Stat()
	if err != nil {
		return err
	}

	bctl.Lock()
	err = bctl.BucketStatUpdateNolock(stat)
	bctl.Unlock()

	// run defragmentation scan
	bctl.ScanBuckets()

	return err
}

func FreeSpaceRatio(st *elliptics.StatBackend, content_length uint64) float64 {
	free_space_rate := 1.0 - float64(st.VFS.BackendUsedSize + content_length) / float64(st.VFS.TotalSizeLimit)
	if st.VFS.Avail <= st.VFS.TotalSizeLimit {
		if st.VFS.Avail < content_length {
			free_space_rate = 0
		} else {
			free_space_rate = float64(st.VFS.Avail - content_length) / float64(st.VFS.TotalSizeLimit)
		}
	}

	return free_space_rate
}

func (bctl *BucketCtl) GetBucket(key string, req *http.Request) (bucket *Bucket) {
	s, err := bctl.e.MetadataSession()
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusServiceUnavailable,
			fmt.Sprintf("get-bucket: could not create metadata session: %v", err))
		return bctl.Bucket[rand.Intn(len(bctl.Bucket))]
	}
	defer s.Delete()

	type bucket_stat struct {
		Bucket		*Bucket
		SuccessGroups	[]uint32
		ErrorGroups	[]uint32
		Pain		float64
		Range		float64

		pains		[]float64
		free_rates	[]float64
	}

	stat := make([]*bucket_stat, 0)
	failed := make([]*bucket_stat, 0)

	bctl.RLock()

	for _, b := range bctl.Bucket {
		bs := &bucket_stat {
			Bucket:		b,
			SuccessGroups:	make([]uint32, 0),
			ErrorGroups:	make([]uint32, 0),
			Pain:		0.0,
			Range:		0.0,

			pains:		make([]float64, 0, len(b.Group)),
			free_rates:	make([]float64, 0, len(b.Group)),

		}

		s.SetNamespace(b.Name)

		for group_id, sg := range b.Group {
			st, err := sg.FindStatBackendKey(s, key, group_id)
			if err != nil {
				// there is no statistics for given address+backend, which should host our data
				// do not allow to write into the bucket which contains given address+backend

				bs.ErrorGroups = append(bs.ErrorGroups, group_id)

				bs.Pain += PainNoStats
				continue
			}

			if st.RO {
				bs.ErrorGroups = append(bs.ErrorGroups, group_id)

				bs.Pain += PainStatRO
				continue
			}

			if st.Error.Code != 0 {
				bs.ErrorGroups = append(bs.ErrorGroups, group_id)

				bs.Pain += PainStatError
				continue
			}

			// this is an empty stat structure
			if st.VFS.TotalSizeLimit == 0 || st.VFS.Total  == 0 {
				bs.ErrorGroups = append(bs.ErrorGroups, group_id)

				bs.Pain += PainNoStats
				continue
			}

			free_space_rate := FreeSpaceRatio(st, uint64(req.ContentLength))
			if free_space_rate <= bctl.Conf.Proxy.FreeSpaceRatioHard {
				bs.ErrorGroups = append(bs.ErrorGroups, group_id)

				bs.Pain += PainNoFreeSpaceHard
			} else if free_space_rate <= bctl.Conf.Proxy.FreeSpaceRatioSoft {
				bs.ErrorGroups = append(bs.ErrorGroups, group_id)

				free_space_pain := 1000.0 / (free_space_rate - bctl.Conf.Proxy.FreeSpaceRatioHard)
				bs.Pain += PainNoFreeSpaceSoft + free_space_pain * 5
			} else {
				bs.SuccessGroups = append(bs.SuccessGroups, group_id)

				free_space_pain := 1000.0 / (free_space_rate - bctl.Conf.Proxy.FreeSpaceRatioSoft)
				if free_space_pain >= PainNoFreeSpaceSoft {
					free_space_pain = PainNoFreeSpaceSoft * 0.8
				}

				bs.Pain += free_space_pain
			}

			pp := st.PIDPain()

			bs.Pain += pp
			bs.pains = append(bs.pains, pp)
			bs.free_rates = append(bs.free_rates, free_space_rate)
		}

		total_groups := len(bs.SuccessGroups) + len(bs.ErrorGroups)
		diff := 0
		if len(b.Meta.Groups) > total_groups {
			diff += len(b.Meta.Groups) - total_groups
		}

		bs.Pain += float64(diff) * PainNoGroup

		// calculate discrepancy pain:
		// run over all address+backends in every group in given bucket,
		// sum up number of live records
		// set discrepancy as a maximum difference between number of records among all groups
		var min_records uint64 = 1<<31-1
		var max_records uint64 = 0

		records := make([]uint64, 0)
		for _, sg := range b.Group {
			var r uint64 = 0

			for _, sb := range sg.Ab {
				r += sb.VFS.RecordsTotal - sb.VFS.RecordsRemoved
			}

			records = append(records, r)
		}

		for _, r := range records {
			if r < min_records {
				min_records = r
			}

			if r > max_records {
				max_records = r
			}
		}
		bs.Pain += float64(max_records - min_records) * PainDiscrepancy


		// do not even consider buckets without free space even in one group
		if bs.Pain >= PainNoFreeSpaceHard {
			//log.Printf("find-bucket: url: %s, bucket: %s, content-length: %d, groups: %v, success-groups: %v, error-groups: %v, pain: %f, pains: %v, free_rates: %v: pain is higher than HARD limit\n",
			//	req.URL.String(), b.Name, req.ContentLength, b.Meta.Groups, bs.SuccessGroups, bs.ErrorGroups, bs.Pain,
			//	bs.pains, bs.free_rates)
			failed = append(failed, bs)
			continue
		}

		if bs.Pain != 0 {
			bs.Range = 1.0 / bs.Pain
		} else {
			bs.Range = 1.0
		}

		stat = append(stat, bs)
	}

	bctl.RUnlock()

	// there are no buckets suitable for this request
	// either there is no space in either bucket, or there are no buckets at all
	if len(stat) == 0 {
		str := make([]string, 0)
		for _, bs := range failed {
			str = append(str, fmt.Sprintf("{bucket: %s, success-groups: %v, error-groups: %v, groups: %v, pain: %f, free-rates: %v}",
				bs.Bucket.Name, bs.SuccessGroups, bs.ErrorGroups, bs.Bucket.Meta.Groups, bs.Pain, bs.free_rates))
		}

		log.Printf("find-bucket: url: %s, content-length: %d: there are no suitable buckets: %v",
			req.URL.String(), req.ContentLength, str)
		return nil
	}

	// get rid of buckets without free space if we do have other buckets
	ok_buckets := 0
	nospace_buckets := 0
	for _, bs := range stat {
		if bs.Pain < PainNoFreeSpaceSoft {
			ok_buckets++
		} else {
			nospace_buckets++
		}
	}

	if nospace_buckets != 0 && ok_buckets != 0 {
		tmp := make([]*bucket_stat, 0)
		for _, bs := range stat {
			if bs.Pain < PainNoFreeSpaceSoft {
				tmp = append(tmp, bs)
			}
		}

		stat = tmp
	}

	str := make([]string, 0)
	for _, bs := range stat {
		str = append(str, fmt.Sprintf("{bucket: %s, success-groups: %v, error-groups: %v, groups: %v, pain: %f, free-rates: %v}",
			bs.Bucket.Name, bs.SuccessGroups, bs.ErrorGroups, bs.Bucket.Meta.Groups, bs.Pain, bs.free_rates))
	}

	log.Printf("find-bucket: url: %s, content-length: %d: %v", req.URL.String(), req.ContentLength, str)

	var sum int64 = 0
	for {
		sum = 0
		var multiple int64 = 10

		for _, bs := range stat {
			sum += int64(bs.Range)
		}

		if sum >= multiple {
			break
		} else {
			for _, bs := range stat {
				bs.Range *= float64(multiple)
			}
		}
	}

	r := rand.Int63n(int64(sum))
	for _, bs := range stat {
		r -= int64(bs.Range)
		if r <= 0 {
			log.Printf("find-bucket: url: %s, selected bucket: %s, content-length: %d, groups: %v, success-groups: %v, error-groups: %v, pain: %f, pains: %v, free_rates: %v\n",
				req.URL.String(), bs.Bucket.Name, req.ContentLength,
				bs.Bucket.Meta.Groups, bs.SuccessGroups, bs.ErrorGroups,
				bs.Pain, bs.pains, bs.free_rates)
			return bs.Bucket
		}
	}

	return nil
}

func (bctl *BucketCtl) bucket_upload(bucket *Bucket, key string, req *http.Request) (reply *reply.LookupResult, err error) {
	err = bucket.check_auth(req, BucketAuthWrite)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), errors.ErrorStatus(err),
			fmt.Sprintf("upload: %s", errors.ErrorData(err)))
		return
	}

	lheader, ok := req.Header["Content-Length"]
	if !ok {
		err = errors.NewKeyError(req.URL.String(), http.StatusBadRequest,
			"upload: there is no Content-Length header")
		return
	}

	total_size, err := strconv.ParseUint(lheader[0], 0, 64)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusBadRequest,
			fmt.Sprintf("upload: invalid content length conversion: %v", err))
		return
	}

	if total_size == 0 {
		err = errors.NewKeyError(req.URL.String(), http.StatusBadRequest,
			"upload: attempting to perform invalid zero-length upload")
		return
	}

	s, err := bctl.e.DataSession(req)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusServiceUnavailable,
			fmt.Sprintf("upload: could not create data session: %v", err))
		return
	}
	defer s.Delete()

	s.SetFilter(elliptics.SessionFilterAll)
	s.SetNamespace(bucket.Name)
	s.SetGroups(bucket.Meta.Groups)
	s.SetTimeout(100)

	log.Printf("upload-trace-id: %x: url: %s, bucket: %s, key: %s, id: %s\n",
		s.GetTraceID(), req.URL.String(), bucket.Name, key, s.Transform(key))

	offset, _, err := URIOffsetSize(req)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusBadRequest, fmt.Sprintf("upload: %v", err))
		return
	}

	start := time.Now()

	reply, err = bucket.lookup_serialize(true, s.WriteData(key, req.Body, offset, total_size))

	// PID controller should aim at some destination performance point
	// it can be velocity pf the vehicle or deisred write rate
	//
	// Let's consider our desired control point as number of useconds needed to write 1 byte into the storage
	// In the ideal world it would be zero

	time_us := time.Since(start).Nanoseconds() / 1000
	e := float64(time_us) / float64(total_size)

	bctl.RLock()

	str := make([]string, 0)
	for _, res := range reply.Servers {
		sg, ok := bucket.Group[res.Group]
		if ok {
			st, back_err := sg.FindStatBackend(res.Addr, res.Backend)
			if back_err == nil {
				old_pain := st.PIDPain()
				update_pain := e
				estring := "ok"

				if res.Error != nil {
					update_pain = BucketWriteErrorPain
					estring = res.Error.Error()
				}
				st.PIDUpdate(update_pain)

				str = append(str, fmt.Sprintf("{group: %d, time: %d us, e: %f, error: %v, pain: %f -> %f}",
					res.Group, time_us, e, estring, old_pain, st.PIDPain()))
			} else {
				str = append(str, fmt.Sprintf("{group: %d, time: %d us, e: %f, error: no backend stat}",
					res.Group, time_us, e))
			}
		} else {
			str = append(str, fmt.Sprintf("{group: %d, time: %d us, e: %f, error: no group stat}",
				res.Group, time_us, e))
		}
	}

	if len(reply.SuccessGroups) == 0 {
		for _, group_id := range bucket.Meta.Groups {
			str = append(str, fmt.Sprintf("{error-group: %d, time: %d us}", group_id, time_us))
		}
	}

	bctl.RUnlock()

	log.Printf("bucket-upload: bucket: %s, key: %s, size: %d: %v\n", bucket.Name, key, total_size, str)

	return
}

func (bctl *BucketCtl) Upload(key string, req *http.Request) (reply *reply.LookupResult, bucket *Bucket, err error) {
	bucket = bctl.GetBucket(key, req)
	if bucket == nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusServiceUnavailable,
			fmt.Sprintf("there are no buckets with free space available"))
		return
	}

	reply, err = bctl.bucket_upload(bucket, key, req)
	return
}

func (bctl *BucketCtl) BucketUpload(bucket_name, key string, req *http.Request) (reply *reply.LookupResult, bucket *Bucket, err error) {
	bucket, err = bctl.FindBucket(bucket_name)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusBadRequest, err.Error())
		return
	}

	reply, err = bctl.bucket_upload(bucket, key, req)
	return
}

func (bctl *BucketCtl) Get(bname, key string, req *http.Request) (resp []byte, err error) {
	bucket, err := bctl.FindBucket(bname)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusBadRequest, err.Error())
		return
	}

	err = bucket.check_auth(req, BucketAuthEmpty)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), errors.ErrorStatus(err),
			fmt.Sprintf("get: %s", errors.ErrorData(err)))
		return
	}

	s, err := bctl.e.DataSession(req)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusServiceUnavailable,
			fmt.Sprintf("get: could not create data session: %v", err))
		return
	}
	defer s.Delete()

	s.SetNamespace(bucket.Name)
	s.SetGroups(bucket.Meta.Groups)

	log.Printf("get-trace-id: %x: url: %s, bucket: %s, key: %s, id: %s\n",
		s.GetTraceID(), req.URL.String(), bucket.Name, key, s.Transform(key))

	offset, size, err := URIOffsetSize(req)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusBadRequest, fmt.Sprintf("get: %v", err))
		return
	}

	for rd := range s.ReadData(key, offset, size) {
		err = rd.Error()
		if err != nil {
			err = errors.NewKeyErrorFromEllipticsError(rd.Error(), req.URL.String(),
				"get: could not read data")
			continue
		}

		resp = rd.Data()
		return
	}
	return
}

func (bctl *BucketCtl) Stream(bname, key string, w http.ResponseWriter, req *http.Request) (err error) {
	bucket, err := bctl.FindBucket(bname)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusBadRequest, err.Error())
		return
	}

	err = bucket.check_auth(req, BucketAuthEmpty)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), errors.ErrorStatus(err),
			fmt.Sprintf("stream: %s", errors.ErrorData(err)))
		return
	}


	s, err := bctl.e.DataSession(req)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusServiceUnavailable,
			fmt.Sprintf("stream: could not create data session: %v", err))
		return
	}
	defer s.Delete()

	s.SetNamespace(bucket.Name)
	s.SetGroups(bucket.Meta.Groups)

	log.Printf("stream-trace-id: %x: url: %s, bucket: %s, key: %s, id: %s\n",
		s.GetTraceID(), req.URL.String(), bucket.Name, key, s.Transform(key))

	offset, size, err := URIOffsetSize(req)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusBadRequest, fmt.Sprintf("stream: %v", err))
		return
	}

	err = s.StreamHTTP(key, offset, size, w)
	if err != nil {
		err = errors.NewKeyErrorFromEllipticsError(err, req.URL.String(), "stream: could not stream data")
		return
	}

	return
}


func (bctl *BucketCtl) Lookup(bname, key string, req *http.Request) (reply *reply.LookupResult, err error) {
	bucket, err := bctl.FindBucket(bname)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusBadRequest, err.Error())
		return
	}

	err = bucket.check_auth(req, BucketAuthEmpty)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), errors.ErrorStatus(err),
			fmt.Sprintf("upload: %s", errors.ErrorData(err)))
		return
	}


	s, err := bctl.e.DataSession(req)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusServiceUnavailable,
			fmt.Sprintf("lookup: could not create data session: %v", err))
		return
	}
	defer s.Delete()

	s.SetNamespace(bucket.Name)
	s.SetGroups(bucket.Meta.Groups)

	log.Printf("lookup-trace-id: %x: url: %s, bucket: %s, key: %s, id: %s\n",
		s.GetTraceID(), req.URL.String(), bucket.Name, key, s.Transform(key))

	reply, err = bucket.lookup_serialize(false, s.ParallelLookup(key))
	return
}

func (bctl *BucketCtl) Delete(bname, key string, req *http.Request) (err error) {
	bucket, err := bctl.FindBucket(bname)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusBadRequest, err.Error())
		return
	}

	err = bucket.check_auth(req, BucketAuthWrite)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), errors.ErrorStatus(err),
			fmt.Sprintf("upload: %s", errors.ErrorData(err)))
		return
	}


	s, err := bctl.e.DataSession(req)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusServiceUnavailable,
			fmt.Sprintf("delete: could not create data session: %v", err))
		return
	}
	defer s.Delete()

	s.SetNamespace(bucket.Name)
	s.SetGroups(bucket.Meta.Groups)

	log.Printf("delete-trace-id: %x: url: %s, bucket: %s, key: %s, id: %s\n",
		s.GetTraceID(), req.URL.String(), bucket.Name, key, s.Transform(key))

	for r := range s.Remove(key) {
		err = r.Error()
	}

	return
}

func (bctl *BucketCtl) BulkDelete(bname string, keys []string, req *http.Request) (reply map[string]interface{}, err error) {
	reply = make(map[string]interface{})

	bucket, err := bctl.FindBucket(bname)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusBadRequest, err.Error())
		return
	}

	err = bucket.check_auth(req, BucketAuthWrite)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), errors.ErrorStatus(err),
			fmt.Sprintf("upload: %s", errors.ErrorData(err)))
		return
	}


	s, err := bctl.e.DataSession(req)
	if err != nil {
		err = errors.NewKeyError(req.URL.String(), http.StatusServiceUnavailable,
			fmt.Sprintf("bulk_delete: could not create data session: %v", err))
		return
	}
	defer s.Delete()

	s.SetNamespace(bucket.Name)
	s.SetGroups(bucket.Meta.Groups)

	log.Printf("bulk-delete-trace-id: %x: url: %s, bucket: %s, keys: %v\n",
		s.GetTraceID(), req.URL.String(), bucket.Name, keys)

	for r := range s.BulkRemove(keys) {
		err = r.Error()
		if err != nil {
			reply[r.Key()] = err.Error()
		}
	}

	err = nil

	return
}

type BucketStat struct {
	Group		map[string]*elliptics.StatGroupData
	Meta		*BucketMsgpack
}

type BctlStat struct {
	Buckets		map[string]*BucketStat
	StatTime	string
}

func (bctl *BucketCtl) Stat(req *http.Request) (reply *BctlStat, err error) {
	bctl.RLock()
	defer bctl.RUnlock()

	reply = &BctlStat {
		Buckets:		make(map[string]*BucketStat),
		StatTime:		bctl.StatTime.String(),
	}

	for _, b := range bctl.AllBuckets() {
		bs := &BucketStat {
			Group:	make(map[string]*elliptics.StatGroupData),
			Meta:	&b.Meta,
		}

		for group, sg := range b.Group {
			sg_data := sg.StatGroupData()
			bs.Group[fmt.Sprintf("%d", group)] = sg_data
		}

		reply.Buckets[b.Name] = bs
	}

	return
}

func (bctl *BucketCtl) ReadBucketConfig() error {
	data, err := ioutil.ReadFile(bctl.bucket_path)
	if err != nil {
		err = fmt.Errorf("Could not read bucket file '%s': %v", bctl.bucket_path, err)
		log.Printf("config: %v\n", err)
		return err
	}

	new_buckets := make([]*Bucket, 0, 0)

	for _, name := range strings.Split(string(data), "\n") {
		if len(name) > 0 {
			b, err := ReadBucket(bctl.e, name)
			if err != nil {
				log.Printf("config: could not read bucket: %s: %v\n", name, err)
				continue
			}

			new_buckets = append(new_buckets, b)
			log.Printf("config: new bucket: %s\n", b.Meta.String())
		}
	}

	if len(new_buckets) == 0 {
		err = fmt.Errorf("No buckets found in bucket file '%s'", bctl.bucket_path)
		log.Printf("config: %v\n", err)
		return err
	}

	stat, err := bctl.e.Stat()
	if err != nil {
		return err
	}

	bctl.Lock()
	bctl.Bucket = new_buckets
	err = bctl.BucketStatUpdateNolock(stat)
	bctl.Unlock()

	log.Printf("Bucket config has been updated, there are %d writable buckets\n", len(new_buckets))
	return nil
}

func (bctl *BucketCtl) ReadProxyConfig() error {
	conf := &config.ProxyConfig {}
	err := conf.Load(bctl.proxy_config_path)
	if err != nil {
		return fmt.Errorf("could not load proxy config file '%s': %v", bctl.proxy_config_path, err)
	}

	bctl.Lock()
	bctl.Conf = conf
	bctl.Unlock()

	if bctl.e.LogFile != nil {
		bctl.e.LogFile.Close()
	}

	bctl.e.LogFile, err = os.OpenFile(conf.Elliptics.LogFile, os.O_RDWR | os.O_APPEND | os.O_CREATE, 0644)
	if err != nil {
		log.Fatalf("Could not open log file '%s': %q", conf.Elliptics.LogFile, err)
	}

	log.SetPrefix(conf.Elliptics.LogPrefix)
	log.SetOutput(bctl.e.LogFile)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	log.Printf("Proxy config has been updated\n")
	return nil

}

func (bctl *BucketCtl) ReadConfig() error {
	err := bctl.ReadBucketConfig()
	if err != nil {
		return fmt.Errorf("failed to update bucket config: %v", err)
	}

	err = bctl.ReadProxyConfig()
	if err != nil {
		return fmt.Errorf("failed to update proxy config: %v", err)
	}

	return nil
}

func (bctl *BucketCtl) ReadBucketsMetaNolock(names []string) (new_buckets []*Bucket, err error) {
	new_buckets = make([]*Bucket, 0, len(names))

	for _, name := range names {
		rb, err := ReadBucket(bctl.e, name)
		if err != nil {
			continue
		}

		new_buckets = append(new_buckets, rb)
	}

	if len(new_buckets) == 0 {
		new_buckets = nil
		err = fmt.Errorf("read-buckets-meta: could not read any bucket from %d requested", len(names))
		return
	}

	return
}

func NewBucketCtl(ell *etransport.Elliptics, bucket_path, proxy_config_path string) (bctl *BucketCtl, err error) {
	bctl = &BucketCtl {
		e:			ell,
		bucket_path:		bucket_path,
		proxy_config_path:	proxy_config_path,
		signals:		make(chan os.Signal, 1),

		Bucket:			make([]*Bucket, 0, 10),
		BackBucket:		make([]*Bucket, 0, 10),

		BucketTimer:		time.NewTimer(time.Second * 30),
		BucketStatTimer:	time.NewTimer(time.Second * 10),

		DefragTime:		time.Now(),
	}

	runtime.SetBlockProfileRate(1000)

	err = bctl.ReadConfig()
	if err != nil {
		return
	}

	signal.Notify(bctl.signals, syscall.SIGHUP)

	go func() {
		for {
			if len(bctl.Conf.Proxy.Root) != 0 {
				file, err := os.OpenFile(bctl.Conf.Proxy.Root + "/" + ProfilePath, os.O_RDWR | os.O_TRUNC | os.O_CREATE, 0644)
				if err != nil {
					return
				}

				fmt.Fprintf(file, "profile dump: %s\n", time.Now().String())
				pprof.Lookup("block").WriteTo(file, 2)

				file.Close()
			}

			time.Sleep(30 * time.Second)
		}
	}()

	go func() {
		for {
			select {
			case <-bctl.BucketTimer.C:
				bctl.ReadConfig()

				if bctl.Conf.Proxy.BucketUpdateInterval > 0 {
					bctl.RLock()
					bctl.BucketTimer.Reset(time.Second * time.Duration(bctl.Conf.Proxy.BucketUpdateInterval))
					bctl.RUnlock()
				}

			case <-bctl.BucketStatTimer.C:
				bctl.BucketStatUpdate()

				if bctl.Conf.Proxy.BucketStatUpdateInterval > 0 {
					bctl.RLock()
					bctl.BucketStatTimer.Reset(time.Second * time.Duration(bctl.Conf.Proxy.BucketStatUpdateInterval))
					bctl.RUnlock()
				}

			case <-bctl.signals:
				// reread config and clean back/read-only bucket list
				bctl.ReadConfig()

				bctl.Lock()
				bctl.BackBucket = make([]*Bucket, 0, 10)
				bctl.Unlock()
			}
		}
	}()

	return bctl, nil
}
