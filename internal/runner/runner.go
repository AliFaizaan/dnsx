package runner

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"github.com/projectdiscovery/clistats"
	"github.com/projectdiscovery/dnsx/libs/dnsx"
	"github.com/projectdiscovery/fileutil"
	"github.com/projectdiscovery/goconfig"
	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/hmap/store/hybrid"
	"github.com/projectdiscovery/iputil"
	"github.com/projectdiscovery/mapcidr"
	retryabledns "github.com/projectdiscovery/retryabledns"
	"go.uber.org/ratelimit"
)

// Runner is a client for running the enumeration process.
type Runner struct {
	options             *Options
	dnsx                *dnsx.DNSX
	wgoutputworker      *sync.WaitGroup
	wgresolveworkers    *sync.WaitGroup
	wgwildcardworker    *sync.WaitGroup
	workerchan          chan string
	outputchan          chan string
	wildcardworkerchan  chan string
	wildcards           map[string]struct{}
	wildcardsmutex      sync.RWMutex
	wildcardscache      map[string][]string
	wildcardscachemutex sync.Mutex
	limiter             ratelimit.Limiter
	hm                  *hybrid.HybridMap
	stats               clistats.StatisticsClient
}

func New(options *Options) (*Runner, error) {
	retryabledns.CheckInternalIPs = true

	dnsxOptions := dnsx.DefaultOptions
	dnsxOptions.MaxRetries = options.Retries
	dnsxOptions.TraceMaxRecursion = options.TraceMaxRecursion
	dnsxOptions.Hostsfile = options.HostsFile

	if options.Resolvers != "" {
		dnsxOptions.BaseResolvers = []string{}
		// If it's a file load resolvers from it
		if fileutil.FileExists(options.Resolvers) {
			rs, err := linesInFile(options.Resolvers)
			if err != nil {
				gologger.Fatal().Msgf("%s\n", err)
			}
			for _, rr := range rs {
				dnsxOptions.BaseResolvers = append(dnsxOptions.BaseResolvers, prepareResolver(rr))
			}
		} else {
			// otherwise gets comma separated ones
			for _, rr := range strings.Split(options.Resolvers, ",") {
				dnsxOptions.BaseResolvers = append(dnsxOptions.BaseResolvers, prepareResolver(rr))
			}
		}
	}

	var questionTypes []uint16
	if options.A {
		questionTypes = append(questionTypes, dns.TypeA)
	}
	if options.AAAA {
		questionTypes = append(questionTypes, dns.TypeAAAA)
	}
	if options.CNAME {
		questionTypes = append(questionTypes, dns.TypeCNAME)
	}
	if options.PTR {
		questionTypes = append(questionTypes, dns.TypePTR)
	}
	if options.SOA {
		questionTypes = append(questionTypes, dns.TypeSOA)
	}
	if options.TXT {
		questionTypes = append(questionTypes, dns.TypeTXT)
	}
	if options.MX {
		questionTypes = append(questionTypes, dns.TypeMX)
	}
	if options.NS {
		questionTypes = append(questionTypes, dns.TypeNS)
	}
	// If no option is specified or wildcard filter has been requested use query type A
	if len(questionTypes) == 0 || options.WildcardDomain != "" {
		options.A = true
		questionTypes = append(questionTypes, dns.TypeA)
	}
	dnsxOptions.QuestionTypes = questionTypes

	dnsX, err := dnsx.New(dnsxOptions)
	if err != nil {
		return nil, err
	}

	limiter := ratelimit.NewUnlimited()
	if options.RateLimit > 0 {
		limiter = ratelimit.New(options.RateLimit)
	}

	hm, err := hybrid.New(hybrid.DefaultDiskOptions)
	if err != nil {
		return nil, err
	}

	var stats clistats.StatisticsClient
	if options.ShowStatistics {
		stats, err = clistats.New()
		if err != nil {
			return nil, err
		}
	}

	r := Runner{
		options:            options,
		dnsx:               dnsX,
		wgoutputworker:     &sync.WaitGroup{},
		wgresolveworkers:   &sync.WaitGroup{},
		wgwildcardworker:   &sync.WaitGroup{},
		workerchan:         make(chan string),
		wildcardworkerchan: make(chan string),
		wildcards:          make(map[string]struct{}),
		wildcardscache:     make(map[string][]string),
		limiter:            limiter,
		hm:                 hm,
		stats:              stats,
	}

	return &r, nil
}

func (r *Runner) InputWorkerStream() {
	var sc *bufio.Scanner
	// attempt to load list from file
	if fileutil.FileExists(r.options.Hosts) {
		f, _ := os.Open(r.options.Hosts)
		sc = bufio.NewScanner(f)
	} else if fileutil.HasStdin() {
		sc = bufio.NewScanner(os.Stdin)
	}

	for sc.Scan() {
		item := strings.TrimSpace(sc.Text())

		hosts := []string{item}
		if iputil.IsCIDR(item) {
			hosts, _ = mapcidr.IPAddresses(item)
		}

		for _, host := range hosts {
			r.workerchan <- host
		}
	}
	close(r.workerchan)
}

func (r *Runner) InputWorker() {
	r.hm.Scan(func(k, _ []byte) error {
		if r.options.ShowStatistics {
			r.stats.IncrementCounter("requests", len(r.dnsx.Options.QuestionTypes))
		}
		item := string(k)
		if r.options.resumeCfg != nil {
			r.options.resumeCfg.current = item
			r.options.resumeCfg.currentIndex++
			if r.options.resumeCfg.currentIndex <= r.options.resumeCfg.Index {
				return nil
			}
		}
		r.workerchan <- item
		return nil
	})
	close(r.workerchan)
}

func (r *Runner) prepareInput() error {
	var dataDomains []byte
	var sc *bufio.Scanner

	// prepare wordlist
	var prefixs []string
	if r.options.WordList != "" {
		dataWordList, err := preProcessArgument(r.options.WordList)
		if err != nil {
			return err
		}
		prefixs = normalizeToSlice(dataWordList)
	}

	if r.options.Domains != "" {
		var err error
		dataDomains, err = preProcessArgument(r.options.Domains)
		if err != nil {
			return err
		}
		sc = bufio.NewScanner(bytes.NewReader(dataDomains))
	}

	if sc == nil {
		// attempt to load list from file
		if fileutil.FileExists(r.options.Hosts) {
			f, err := os.Open(r.options.Hosts)
			if err != nil {
				return err
			}
			sc = bufio.NewScanner(f)
		} else if argumentHasStdin(r.options.Hosts) || hasStdin() {
			sc = bufio.NewScanner(os.Stdin)
		} else {
			return errors.New("hosts file or stdin not provided")
		}
	}

	numHosts := 0
	for sc.Scan() {
		item := strings.TrimSpace(sc.Text())
		var hosts []string
		switch {
		case r.options.WordList != "":
			for _, prefix := range prefixs {
				// domains Cartesian product with wordlist
				subdomain := strings.TrimSpace(prefix) + "." + item
				hosts = append(hosts, subdomain)
			}
		case iputil.IsCIDR(item):
			hosts, _ = mapcidr.IPAddresses(item)
		default:
			hosts = []string{item}
		}

		for _, host := range hosts {
			// Used just to get the exact number of targets
			if _, ok := r.hm.Get(host); ok {
				continue
			}
			numHosts++
			// nolint:errcheck
			r.hm.Set(host, nil)
		}
	}

	if r.options.ShowStatistics {
		r.stats.AddStatic("hosts", numHosts)
		r.stats.AddStatic("startedAt", time.Now())
		r.stats.AddCounter("requests", 0)
		r.stats.AddCounter("total", uint64(numHosts*len(r.dnsx.Options.QuestionTypes)))
		// nolint:errcheck
		r.stats.Start(makePrintCallback(), time.Duration(5)*time.Second)
	}

	return nil
}

func hasStdin() bool {
	stat, _ := os.Stdin.Stat()
	return (stat.Mode() & os.ModeCharDevice) == 0
}

func preProcessArgument(arg string) ([]byte, error) {
	var (
		data []byte
		err  error
	)
	// read from:
	// file
	switch {
	case fileutil.FileExists(arg):
		data, err = os.ReadFile(arg)
		if err != nil {
			return nil, err
		}
	// stdin
	case argumentHasStdin(arg):
		data, err = ioutil.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
	// inline
	case arg != "":
		data = []byte(arg)
	default:
		return nil, errors.New("empty argument")
	}

	return bytes.Replace(data, []byte(Comma), []byte(NewLine), -1), nil
}

func normalizeToSlice(data []byte) []string {
	var s []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		item := strings.TrimSpace(sc.Text())
		s = append(s, item)
	}
	return s
}

// nolint:deadcode
func makePrintCallback() func(stats clistats.StatisticsClient) {
	builder := &strings.Builder{}
	return func(stats clistats.StatisticsClient) {
		builder.WriteRune('[')
		startedAt, _ := stats.GetStatic("startedAt")
		duration := time.Since(startedAt.(time.Time))
		builder.WriteString(fmtDuration(duration))
		builder.WriteRune(']')

		hosts, _ := stats.GetStatic("hosts")
		builder.WriteString(" | Hosts: ")
		builder.WriteString(clistats.String(hosts))

		requests, _ := stats.GetCounter("requests")
		total, _ := stats.GetCounter("total")

		builder.WriteString(" | RPS: ")
		builder.WriteString(clistats.String(uint64(float64(requests) / duration.Seconds())))

		builder.WriteString(" | Requests: ")
		builder.WriteString(clistats.String(requests))
		builder.WriteRune('/')
		builder.WriteString(clistats.String(total))
		builder.WriteRune(' ')
		builder.WriteRune('(')
		//nolint:gomnd // this is not a magic number
		builder.WriteString(clistats.String(uint64(float64(requests) / float64(total) * 100.0)))
		builder.WriteRune('%')
		builder.WriteRune(')')
		builder.WriteRune('\n')

		fmt.Fprintf(os.Stderr, "%s", builder.String())
		builder.Reset()
	}
}

// SaveResumeConfig to file
func (r *Runner) SaveResumeConfig() error {
	var resumeCfg ResumeCfg
	resumeCfg.Index = r.options.resumeCfg.currentIndex
	resumeCfg.ResumeFrom = r.options.resumeCfg.current
	return goconfig.Save(resumeCfg, DefaultResumeFile)
}

func (r *Runner) Run() error {
	if r.options.Stream {
		return r.runStream()
	}

	return r.run()
}

func (r *Runner) run() error {
	err := r.prepareInput()
	if err != nil {
		return err
	}

	// if resume is enabled inform the user
	if r.options.ShouldLoadResume() && r.options.resumeCfg.Index > 0 {
		gologger.Debug().Msgf("Resuming scan using file %s. Restarting at position %d: %s\n", DefaultResumeFile, r.options.resumeCfg.Index, r.options.resumeCfg.ResumeFrom)
	}

	r.startWorkers()

	r.wgresolveworkers.Wait()
	if r.stats != nil {
		err = r.stats.Stop()
		if err != nil {
			return err
		}
	}

	close(r.outputchan)
	r.wgoutputworker.Wait()

	if r.options.WildcardDomain != "" {
		gologger.Print().Msgf("Starting to filter wildcard subdomains\n")
		ipDomain := make(map[string]map[string]struct{})
		listIPs := []string{}
		// prepare in memory structure similarly to shuffledns
		r.hm.Scan(func(k, v []byte) error {
			var dnsdata retryabledns.DNSData
			err := dnsdata.Unmarshal(v)
			if err != nil {
				// the item has no record - ignore
				return nil
			}

			for _, a := range dnsdata.A {
				_, ok := ipDomain[a]
				if !ok {
					ipDomain[a] = make(map[string]struct{})
					listIPs = append(listIPs, a)
				}
				ipDomain[a][string(k)] = struct{}{}
			}

			return nil
		})

		// wildcard workers
		numThreads := r.options.Threads
		if numThreads > len(listIPs) {
			numThreads = len(listIPs)
		}
		for i := 0; i < numThreads; i++ {
			r.wgwildcardworker.Add(1)
			go r.wildcardWorker()
		}

		seen := make(map[string]struct{})
		for _, a := range listIPs {
			hosts := ipDomain[a]
			if len(hosts) >= r.options.WildcardThreshold {
				for host := range hosts {
					if _, ok := seen[host]; !ok {
						seen[host] = struct{}{}
						r.wildcardworkerchan <- host
					}
				}
			}
		}
		close(r.wildcardworkerchan)
		r.wgwildcardworker.Wait()

		// we need to restart output
		r.startOutputWorker()
		seen = make(map[string]struct{})
		seenRemovedSubdomains := make(map[string]struct{})
		numRemovedSubdomains := 0
		for _, A := range listIPs {
			for host := range ipDomain[A] {
				if host == r.options.WildcardDomain {
					if _, ok := seen[host]; !ok {
						seen[host] = struct{}{}
						r.outputchan <- host
					}
				} else if _, ok := r.wildcards[host]; !ok {
					if _, ok := seen[host]; !ok {
						seen[host] = struct{}{}
						r.outputchan <- host
					}
				} else {
					if _, ok := seenRemovedSubdomains[host]; !ok {
						numRemovedSubdomains++
						seenRemovedSubdomains[host] = struct{}{}
					}
				}
			}
		}
		close(r.outputchan)
		// waiting output worker
		r.wgoutputworker.Wait()
		gologger.Print().Msgf("%d wildcard subdomains removed\n", numRemovedSubdomains)
	}

	return nil
}

func (r *Runner) runStream() error {
	r.startWorkers()

	r.wgresolveworkers.Wait()

	close(r.outputchan)
	r.wgoutputworker.Wait()

	return nil
}

func (r *Runner) HandleOutput() {
	defer r.wgoutputworker.Done()

	// setup output
	var (
		foutput *os.File
		w       *bufio.Writer
	)
	if r.options.OutputFile != "" {
		var err error
		foutput, err = os.OpenFile(r.options.OutputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			gologger.Fatal().Msgf("%s\n", err)
		}
		defer foutput.Close()
		w = bufio.NewWriter(foutput)
		defer w.Flush()
		var flushTicker *time.Ticker
		if r.options.FlushInterval >= 0 {
			flushTicker = time.NewTicker(time.Duration(r.options.FlushInterval) * time.Second)
			defer flushTicker.Stop()
			go func() {
				for range flushTicker.C {
					w.Flush()
				}
			}()
		}
	}
	for item := range r.outputchan {
		if r.options.OutputFile != "" {
			// uses a buffer to write to file
			// nolint:errcheck
			w.WriteString(item + "\n")
		}
		// otherwise writes sequentially to stdout
		gologger.Silent().Msgf("%s\n", item)
	}
}

func (r *Runner) startOutputWorker() {
	// output worker
	r.outputchan = make(chan string)
	r.wgoutputworker.Add(1)
	go r.HandleOutput()
}

func (r *Runner) startWorkers() {
	if r.options.Stream {
		go r.InputWorkerStream()
	} else {
		go r.InputWorker()
	}

	r.startOutputWorker()
	// resolve workers
	for i := 0; i < r.options.Threads; i++ {
		r.wgresolveworkers.Add(1)
		go r.worker()
	}
}

func (r *Runner) worker() {
	defer r.wgresolveworkers.Done()

	for domain := range r.workerchan {
		if isURL(domain) {
			domain = extractDomain(domain)
		}
		r.limiter.Take()

		// Ignoring errors as partial results are still good
		dnsData, _ := r.dnsx.QueryMultiple(domain)
		// Just skipping nil responses (in case of critical errors)
		if dnsData == nil {
			continue
		}

		if dnsData.Host == "" || dnsData.Timestamp.IsZero() {
			continue
		}

		// skip responses not having the expected response code
		if len(r.options.rcodes) > 0 {
			if _, ok := r.options.rcodes[dnsData.StatusCodeRaw]; !ok {
				continue
			}
		}

		if !r.options.Raw {
			dnsData.Raw = ""
		}

		if r.options.Trace {
			dnsData.TraceData, _ = r.dnsx.Trace(domain)
			if dnsData.TraceData != nil {
				for _, data := range dnsData.TraceData.DNSData {
					if r.options.Raw && data.RawResp != nil {
						rawRespString := data.RawResp.String()
						data.Raw = rawRespString
						// join the whole chain in raw field
						dnsData.Raw += fmt.Sprintln(rawRespString)
					}
					data.RawResp = nil
				}
			}
		}

		// if wildcard filtering just store the data
		if r.options.WildcardDomain != "" {
			// nolint:errcheck
			r.storeDNSData(dnsData)
			continue
		}
		if r.options.JSON {
			jsons, _ := dnsData.JSON()
			r.outputchan <- jsons
			continue
		}
		if r.options.Raw {
			r.outputchan <- dnsData.Raw
			continue
		}
		if r.options.hasRCodes {
			r.outputResponseCode(domain, dnsData.StatusCodeRaw)
			continue
		}
		if r.options.A {
			r.outputRecordType(domain, dnsData.A)
		}
		if r.options.AAAA {
			r.outputRecordType(domain, dnsData.AAAA)
		}
		if r.options.CNAME {
			r.outputRecordType(domain, dnsData.CNAME)
		}
		if r.options.PTR {
			r.outputRecordType(domain, dnsData.PTR)
		}
		if r.options.MX {
			r.outputRecordType(domain, dnsData.MX)
		}
		if r.options.NS {
			r.outputRecordType(domain, dnsData.NS)
		}
		if r.options.SOA {
			r.outputRecordType(domain, dnsData.SOA)
		}
		if r.options.TXT {
			r.outputRecordType(domain, dnsData.TXT)
		}
	}
}

func (r *Runner) outputRecordType(domain string, items []string) {
	for _, item := range items {
		item := strings.ToLower(item)
		if r.options.ResponseOnly {
			r.outputchan <- item
		} else if r.options.Response {
			r.outputchan <- domain + " [" + item + "]"
		} else {
			// just prints out the domain if it has a record type and exit
			r.outputchan <- domain
			break
		}
	}
}

func (r *Runner) outputResponseCode(domain string, responsecode int) {
	responseCodeExt, ok := dns.RcodeToString[responsecode]
	if ok {
		r.outputchan <- domain + " [" + responseCodeExt + "]"
	}
}

func (r *Runner) storeDNSData(dnsdata *retryabledns.DNSData) error {
	data, err := dnsdata.Marshal()
	if err != nil {
		return err
	}
	return r.hm.Set(dnsdata.Host, data)
}

// Close running instance
func (r *Runner) Close() {
	r.hm.Close()
}

func (r *Runner) wildcardWorker() {
	defer r.wgwildcardworker.Done()

	for {
		host, more := <-r.wildcardworkerchan
		if !more {
			break
		}

		if r.IsWildcard(host) {
			// mark this host as a wildcard subdomain
			r.wildcardsmutex.Lock()
			r.wildcards[host] = struct{}{}
			r.wildcardsmutex.Unlock()
		}
	}
}
