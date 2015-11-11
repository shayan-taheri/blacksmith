package dhcp

import (
	"encoding/json"
	"errors"
	"net"
	"path"
	"sync"
	"time"

	"github.com/coreos/etcd/Godeps/_workspace/src/golang.org/x/net/context"
	etcd "github.com/coreos/etcd/client"
	"github.com/krolaw/dhcp4"
)

// TODO simplify assign and refresh
// 		use quorom?
//		better locking mechanism
//		tests
//		check ips if startip and range changed

var (
	ErrLeasePoolIsFull   = errors.New("there is no empty IP address at the moment")
	ErrRefreshNoMatch    = errors.New("there is no match between specified ip and nic")
	ErrFoundInvalidLease = errors.New("there is an invalid lease in datasource")
)

type Lease struct {
	Nic           string
	IP            net.IP
	FirstAssigned time.Time
	LastAssigned  time.Time
	ExpireTime    time.Time
}

func newLease(nic string, ip net.IP, expireDuration time.Duration, firstAssigned *time.Time) Lease {
	now := time.Now()
	lease := Lease{
		Nic:          nic,
		IP:           ip,
		LastAssigned: now,
		ExpireTime:   now.Add(expireDuration),
	}
	if firstAssigned == nil {
		lease.FirstAssigned = now
	} else {
		lease.FirstAssigned = *firstAssigned
	}
	return lease
}

type LeasePool struct {
	etcdDir        string
	startIP        net.IP
	rangeLen       int
	expireDuration time.Duration
	dataSource     etcd.KeysAPI
	dataLock       sync.Mutex
	assignLock     sync.Mutex
}

func NewLeasePool(dataSource etcd.KeysAPI, etcdDir string, startIP net.IP, rangeLen int, expireDuration time.Duration) (*LeasePool, error) {
	pool := &LeasePool{
		etcdDir:        etcdDir,
		startIP:        startIP,
		expireDuration: expireDuration,
		rangeLen:       rangeLen,
		dataSource:     dataSource,
	}
	return pool, nil
}

// Store will store the lease in etcd
func (p *LeasePool) Store(lease Lease) error {
	p.dataLock.Lock()
	defer p.dataLock.Unlock()
	data, err := json.Marshal(lease)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = p.dataSource.Set(ctx, path.Join(p.etcdDir, "/leases", lease.IP.String()), string(data), nil)
	return err
}

// Leases returns map binary.BigEndian.Uint32(IP) and Lease of all assigned leases
func (p *LeasePool) Leases() (map[string]Lease, error) {
	p.dataLock.Lock()
	defer p.dataLock.Unlock()
	leases := make(map[string]Lease, 10)

	ctxGet, cancelGet := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelGet()
	response, err := p.dataSource.Get(ctxGet, path.Join(p.etcdDir, "/leases"), &etcd.GetOptions{Recursive: true})

	if err != nil {
		etcdError, found := err.(etcd.Error)
		if found && etcdError.Code == etcd.ErrorCodeKeyNotFound {
			// handle key not found
			ctxSet, cancelSet := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelSet()
			_, err := p.dataSource.Set(ctxSet, path.Join(p.etcdDir, "/leases"), "", &etcd.SetOptions{Dir: true})
			if err != nil {
				return nil, err
			}
			return leases, nil
		}
		return nil, err
	}
	for i := range response.Node.Nodes {
		var lease Lease
		err := json.Unmarshal([]byte(response.Node.Nodes[i].Value), &lease)
		if err == nil {
			leases[lease.IP.String()] = lease
		} else {
			return nil, ErrFoundInvalidLease
		}
	}
	return leases, nil
}

// Reset will delete all the assigned leases
func (p *LeasePool) Reset() error {
	p.dataLock.Lock()
	defer p.dataLock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := p.dataSource.Delete(ctx, path.Join(p.etcdDir, "/leases"), &etcd.DeleteOptions{Dir: true, Recursive: true})
	if err != nil {
		etcdError, found := err.(etcd.Error)
		if found && etcdError.Code == etcd.ErrorCodeKeyNotFound {
			return nil
		}
		return err
	}
	return nil
}

// Assign will find an IP for the specified nic
func (p *LeasePool) Assign(nic string) (net.IP, error) {
	p.assignLock.Lock()
	defer p.assignLock.Unlock()
	leases, err := p.Leases()
	if err != nil {
		return nil, err
	}
	// try to find by mac address
	for _, lease := range leases {
		if lease.Nic == nic {
			p.Store(newLease(nic, lease.IP, p.expireDuration, &lease.FirstAssigned))
			return lease.IP, nil
		}
	}
	// find an unseen ip
	for i := 0; i < p.rangeLen; i++ {
		ip := dhcp4.IPAdd(p.startIP, i)
		_, exists := leases[ip.String()]
		if !exists {
			err := p.Store(newLease(nic, ip, p.expireDuration, nil))
			if err != nil {
				return nil, err
			}
			return ip, nil
		}
	}
	// find an expired ip
	now := time.Now()
	for _, lease := range leases {
		if lease.ExpireTime.Before(now) {
			p.Store(newLease(nic, lease.IP, p.expireDuration, nil))
			return lease.IP, nil
		}
	}
	return nil, ErrLeasePoolIsFull
}

func (p *LeasePool) Request(nic string, currentIP net.IP) (net.IP, error) {
	p.assignLock.Lock()
	defer p.assignLock.Unlock()
	leases, err := p.Leases()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	lease, exists := leases[currentIP.String()]
	if exists && lease.Nic == nic {
		p.Store(newLease(nic, lease.IP, p.expireDuration, &lease.FirstAssigned))
		return lease.IP, nil
	} else if exists && lease.ExpireTime.After(now) {
		return nil, ErrRefreshNoMatch
	} else if !exists {
		// try to find by mac address
		for _, lease := range leases {
			// there exists an ip for this mac
			if lease.Nic == nic && lease.ExpireTime.After(now) {
				return nil, ErrRefreshNoMatch
			}
		}
		// assign that ip to this nic
		p.Store(newLease(nic, currentIP, p.expireDuration, nil))
		return currentIP, nil
	}
	return nil, ErrRefreshNoMatch
}