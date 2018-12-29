package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	rch "github.com/luomai/kungfu/srcs/go/rchannel"
	"github.com/luomai/kungfu/srcs/go/runner"
	sch "github.com/luomai/kungfu/srcs/go/scheduler"
	"github.com/luomai/kungfu/srcs/go/utils"
	"github.com/luomai/kungfu/srcs/go/wire"
)

var (
	hostList   = flag.String("H", "", "comma separated list of <hostname>:<nslots>[,<public addr>]")
	user       = flag.String("u", "", "user name for ssh")
	timeout    = flag.Duration("timeout", 90*time.Second, "timeout")
	verboseLog = flag.Bool("v", true, "show task log")
)

func main() {
	flag.Parse()
	restArgs := flag.Args()
	if len(restArgs) < 1 {
		utils.ExitErr(errors.New("missing program name"))
	}
	prog := restArgs[0]
	args := restArgs[1:]

	hostSpecs, err := rch.ParseHostSpec(*hostList)
	if err != nil {
		utils.ExitErr(err)
	}
	log.Printf("using VMs: %#v", hostSpecs)
	log.Printf("using host spec: %s", fmtHostSpecs(hostSpecs))

	records := runAllExperiments(hostSpecs, prog, args, *timeout)
	fmt.Printf("all results (%d records):\n", len(records))
	for i, r := range records {
		fmt.Printf("#%d %s\n", i, r)
	}
}

type Record struct {
	Partition []int
	Algo      wire.KungFu_AllReduceAlgo
	Result    Result
}

func (r Record) String() string {
	return fmt.Sprintf("%s %v %s", r.Algo, r.Partition, r.Result)
}

type Result struct {
	Mean float32
	Conf float32
}

func (r Result) String() string {
	return fmt.Sprintf("%f +-%f", r.Mean, r.Conf)
}

func runAllExperiments(hosts []rch.HostSpec, prog string, args []string, timeout time.Duration) []Record {
	pool := make(chan rch.HostSpec, len(hosts))
	for _, h := range hosts {
		pool <- h
	}
	var banker sync.Mutex
	requireN := func(n int) []rch.HostSpec {
		tk := time.NewTicker(1 * time.Second)
		defer tk.Stop()
		for {
			got := func() []rch.HostSpec {
				banker.Lock()
				banker.Unlock()
				if len(pool) >= n {
					var hs []rch.HostSpec
					for i := 0; i < n; i++ {
						hs = append(hs, <-pool)
					}
					return hs
				}
				return nil
			}()
			if got != nil {
				return got
			}
			<-tk.C
		}
	}
	returnAll := func(hs []rch.HostSpec) {
		for _, h := range hs {
			pool <- h
		}
	}

	var wg sync.WaitGroup
	var records []Record
	var lock sync.Mutex
	run := func(algo wire.KungFu_AllReduceAlgo, partition []int) {
		if len(hosts) < len(partition) {
			return // total resource not sufficient
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			hs := requireN(len(partition))
			defer func() { returnAll(hs) }()
			log.Printf("begin experiment {%s %v} on {%s}", algo, partition, humanizeHostSpecs(hs))
			res, err := runExperiment(hs, prog, args, algo, partition, timeout)
			if err != nil {
				log.Printf("failed experiment {%s %v} with: %v", algo, partition, err)
				return
			}
			r := Record{
				Algo:      algo,
				Partition: partition,
				Result:    *res,
			}
			log.Printf("end experiment {%s %v} on {%s} with: %s", algo, partition, humanizeHostSpecs(hs), r)
			lock.Lock()
			records = append(records, r)
			log.Printf("got results from %d experiments", len(records))
			lock.Unlock()
		}()
	}

	algos := []wire.KungFu_AllReduceAlgo{
		wire.KungFu_Simple,
		wire.KungFu_Ring,
		wire.KungFu_Clique,
		wire.KungFu_Tree,
	}
	for _, a := range algos {
		run(a, []int{1})
		run(a, []int{2})
		run(a, []int{3})
		run(a, []int{4})

		run(a, []int{1, 3})
		run(a, []int{2, 2})
		run(a, []int{3, 3})
		run(a, []int{4, 4})
		// run([]int{1, 1, 1, 1})
	}

	wg.Wait()
	return records
}

func runExperiment(hosts []rch.HostSpec, prog string, args []string, algo wire.KungFu_AllReduceAlgo, partition []int, timeout time.Duration) (*Result, error) {
	hosts, err := reschedule(hosts, partition)
	if err != nil {
		return nil, err
	}

	jc := sch.JobConfig{
		TaskCount: rch.TotalCap(hosts),
		HostList:  fmtHostSpecs(hosts),
		Prog:      prog,
		Args:      args,
	}
	ps, err := jc.CreateProcs(algo)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var res Result
	d, err := utils.Measure(func() error {
		results, err := runner.RemoteRunAll(ctx, *user, ps, *verboseLog)
		for _, r := range results {
			if info := grep(`Img/sec per /gpu:0`, r.Stdout); len(info) > 0 {
				parseResult(info[0], &res)
				break
			}
		}
		return err
	})
	log.Printf("all %d tasks finished, took %s", len(ps), d)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func reschedule(hosts []rch.HostSpec, partition []int) ([]rch.HostSpec, error) {
	if len(hosts) < len(partition) {
		return nil, errors.New("hosts not enough")
	}
	var workers []rch.HostSpec
	for i, p := range partition {
		w := hosts[i]
		if w.Slots < p {
			return nil, errors.New("host slots not enough")
		}
		w.Slots = p
		workers = append(workers, w)
	}
	return workers, nil
}

func fmtHostSpecs(hosts []rch.HostSpec) string {
	var ss []string
	for _, h := range hosts {
		ss = append(ss, h.String())
	}
	return strings.Join(ss, ",")
}

func humanizeHostSpecs(hosts []rch.HostSpec) string {
	var ss []string
	for _, h := range hosts {
		ss = append(ss, fmt.Sprintf("<ip=%s, slots=%d, pub_ip=%s>", h.Hostname, h.Slots, h.PublicAddr))
	}
	return strings.Join(ss, ", ")
}

func grep(pattern string, input []string) []string {
	var lines []string
	for _, line := range input {
		if strings.Contains(line, pattern) {
			lines = append(lines, line)
		}
	}
	return lines
}

func parseResult(line string, r *Result) {
	fmt.Sscanf(line, `Img/sec per /gpu:0: %f +-%f`, &r.Mean, &r.Conf)
}
