// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package event

import (
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/tsuru/tsuru/auth"
	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/db/storage"
	"github.com/tsuru/tsuru/log"
	"github.com/tsuru/tsuru/permission"
	"github.com/tsuru/tsuru/safe"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

var (
	lockUpdateInterval = 30 * time.Second
	lockExpireTimeout  = 5 * time.Minute
	updater            = lockUpdater{
		addCh:    make(chan *Target),
		removeCh: make(chan *Target),
		once:     &sync.Once{},
	}
	throttlingInfo  = map[string]ThrottlingSpec{}
	errInvalidQuery = errors.New("invalid query")

	ErrNotCancelable     = errors.New("event is not cancelable")
	ErrEventNotFound     = errors.New("event not found")
	ErrNoTarget          = ErrValidation("event target is mandatory")
	ErrNoKind            = ErrValidation("event kind is mandatory")
	ErrNoOwner           = ErrValidation("event owner is mandatory")
	ErrNoOpts            = ErrValidation("event opts is mandatory")
	ErrNoInternalKind    = ErrValidation("event internal kind is mandatory")
	ErrNoAllowed         = errors.New("event allowed is mandatory")
	ErrNoAllowedCancel   = errors.New("event allowed cancel is mandatory for cancelable events")
	ErrInvalidOwner      = ErrValidation("event owner must not be set on internal events")
	ErrInvalidKind       = ErrValidation("event kind must not be set on internal events")
	ErrInvalidTargetType = errors.New("invalid event target type")

	OwnerTypeUser     = ownerType("user")
	OwnerTypeApp      = ownerType("app")
	OwnerTypeInternal = ownerType("internal")

	KindTypePermission = kindType("permission")
	KindTypeInternal   = kindType("internal")

	TargetTypeApp             = TargetType("app")
	TargetTypeNode            = TargetType("node")
	TargetTypeContainer       = TargetType("container")
	TargetTypePool            = TargetType("pool")
	TargetTypeService         = TargetType("service")
	TargetTypeServiceInstance = TargetType("service-instance")
	TargetTypeTeam            = TargetType("team")
	TargetTypeUser            = TargetType("user")
	TargetTypeIaas            = TargetType("iaas")
	TargetTypeRole            = TargetType("role")
	TargetTypePlatform        = TargetType("platform")
	TargetTypePlan            = TargetType("plan")
	TargetTypeNodeContainer   = TargetType("node-container")
	TargetTypeInstallHost     = TargetType("install-host")
	TargetTypeEventBlock      = TargetType("event-block")
)

const (
	filterMaxLimit = 100
)

type ErrThrottled struct {
	Spec   *ThrottlingSpec
	Target Target
}

func (err ErrThrottled) Error() string {
	var extra string
	if err.Spec.KindName != "" {
		extra = fmt.Sprintf(" %s on", err.Spec.KindName)
	}
	return fmt.Sprintf("event throttled, limit for%s %s %q is %d every %v", extra, err.Target.Type, err.Target.Value, err.Spec.Max, err.Spec.Time)
}

type ErrValidation string

func (err ErrValidation) Error() string {
	return string(err)
}

type ErrEventLocked struct{ event *Event }

func (err ErrEventLocked) Error() string {
	return fmt.Sprintf("event locked: %v", err.event)
}

type Target struct {
	Type  TargetType
	Value string
}

func (id Target) GetBSON() (interface{}, error) {
	return bson.D{{Name: "type", Value: id.Type}, {Name: "value", Value: id.Value}}, nil
}

func (id Target) IsValid() bool {
	return id.Type != ""
}

func (id Target) String() string {
	return fmt.Sprintf("%s(%s)", id.Type, id.Value)
}

type eventID struct {
	Target Target
	ObjId  bson.ObjectId
}

func (id *eventID) SetBSON(raw bson.Raw) error {
	err := raw.Unmarshal(&id.Target)
	if err != nil {
		return raw.Unmarshal(&id.ObjId)
	}
	return nil
}

func (id eventID) GetBSON() (interface{}, error) {
	if len(id.ObjId) != 0 {
		return id.ObjId, nil
	}
	return id.Target.GetBSON()
}

// This private type allow us to export the main Event struct without allowing
// access to its public fields. (They have to be public for database
// serializing).
type eventData struct {
	ID              eventID `bson:"_id"`
	UniqueID        bson.ObjectId
	StartTime       time.Time
	EndTime         time.Time `bson:",omitempty"`
	Target          Target    `bson:",omitempty"`
	StartCustomData bson.Raw  `bson:",omitempty"`
	EndCustomData   bson.Raw  `bson:",omitempty"`
	OtherCustomData bson.Raw  `bson:",omitempty"`
	Kind            Kind
	Owner           Owner
	LockUpdateTime  time.Time
	Error           string
	Log             string    `bson:",omitempty"`
	RemoveDate      time.Time `bson:",omitempty"`
	CancelInfo      cancelInfo
	Cancelable      bool
	Running         bool
	Allowed         AllowedPermission
	AllowedCancel   AllowedPermission
}

type cancelInfo struct {
	Owner     string
	StartTime time.Time
	AckTime   time.Time
	Reason    string
	Asked     bool
	Canceled  bool
}

type ownerType string

type kindType string

type TargetType string

func GetTargetType(t string) (TargetType, error) {
	switch t {
	case "app":
		return TargetTypeApp, nil
	case "node":
		return TargetTypeNode, nil
	case "container":
		return TargetTypeContainer, nil
	case "pool":
		return TargetTypePool, nil
	case "service":
		return TargetTypeService, nil
	case "service-instance":
		return TargetTypeServiceInstance, nil
	case "team":
		return TargetTypeTeam, nil
	case "user":
		return TargetTypeUser, nil
	}
	return TargetType(""), ErrInvalidTargetType
}

type Owner struct {
	Type ownerType
	Name string
}

type Kind struct {
	Type kindType
	Name string
}

func (o Owner) String() string {
	return fmt.Sprintf("%s %s", o.Type, o.Name)
}

func (k Kind) String() string {
	return k.Name
}

type ThrottlingSpec struct {
	TargetType TargetType
	KindName   string
	Max        int
	Time       time.Duration
}

func SetThrottling(spec ThrottlingSpec) {
	key := string(spec.TargetType)
	if spec.KindName != "" {
		key = fmt.Sprintf("%s_%s", spec.TargetType, spec.KindName)
	}
	throttlingInfo[key] = spec
}

func getThrottling(t *Target, k *Kind) *ThrottlingSpec {
	key := fmt.Sprintf("%s_%s", t.Type, k.Name)
	if s, ok := throttlingInfo[key]; ok {
		return &s
	}
	if s, ok := throttlingInfo[string(t.Type)]; ok {
		return &s
	}
	return nil
}

type Event struct {
	eventData
	logBuffer safe.Buffer
	logWriter io.Writer
}

type Opts struct {
	Target        Target
	Kind          *permission.PermissionScheme
	InternalKind  string
	Owner         auth.Token
	RawOwner      Owner
	CustomData    interface{}
	DisableLock   bool
	Cancelable    bool
	Allowed       AllowedPermission
	AllowedCancel AllowedPermission
}

func Allowed(scheme *permission.PermissionScheme, contexts ...permission.PermissionContext) AllowedPermission {
	return AllowedPermission{
		Scheme:   scheme.FullName(),
		Contexts: contexts,
	}
}

type AllowedPermission struct {
	Scheme   string
	Contexts []permission.PermissionContext `bson:",omitempty"`
}

func (ap *AllowedPermission) GetBSON() (interface{}, error) {
	var ctxs []bson.D
	for _, ctx := range ap.Contexts {
		ctxs = append(ctxs, bson.D{
			{Name: "ctxtype", Value: ctx.CtxType},
			{Name: "value", Value: ctx.Value},
		})
	}
	return bson.M{
		"scheme":   ap.Scheme,
		"contexts": ctxs,
	}, nil
}

func (e *Event) String() string {
	return fmt.Sprintf("%s(%s) running %q start by %s at %s",
		e.Target.Type,
		e.Target.Value,
		e.Kind,
		e.Owner,
		e.StartTime.Format(time.RFC3339),
	)
}

type TargetFilter struct {
	Type   TargetType
	Values []string
}

type Filter struct {
	Target         Target
	KindType       kindType
	KindName       string
	OwnerType      ownerType
	OwnerName      string
	Since          time.Time
	Until          time.Time
	Running        *bool
	IncludeRemoved bool
	ErrorOnly      bool
	Raw            bson.M
	AllowedTargets []TargetFilter
	Permissions    []permission.Permission

	Limit int
	Skip  int
	Sort  string
}

func (f *Filter) PruneUserValues() {
	f.Raw = nil
	f.AllowedTargets = nil
	f.Permissions = nil
	if f.Limit > filterMaxLimit || f.Limit <= 0 {
		f.Limit = filterMaxLimit
	}
}

func (f *Filter) toQuery() (bson.M, error) {
	query := bson.M{}
	permMap := map[string][]permission.PermissionContext{}
	if f.Permissions != nil {
		for _, p := range f.Permissions {
			permMap[p.Scheme.FullName()] = append(permMap[p.Scheme.FullName()], p.Context)
		}
		var permOrBlock []bson.M
		for perm, ctxs := range permMap {
			ctxsBson := []bson.D{}
			for _, ctx := range ctxs {
				if ctx.CtxType == permission.CtxGlobal {
					ctxsBson = nil
					break
				}
				ctxsBson = append(ctxsBson, bson.D{
					{Name: "ctxtype", Value: ctx.CtxType},
					{Name: "value", Value: ctx.Value},
				})
			}
			toAppend := bson.M{
				"allowed.scheme": bson.M{"$regex": "^" + strings.Replace(perm, ".", `\.`, -1)},
			}
			if ctxsBson != nil {
				toAppend["allowed.contexts"] = bson.M{"$in": ctxsBson}
			}
			permOrBlock = append(permOrBlock, toAppend)
		}
		query["$or"] = permOrBlock
	}
	if f.AllowedTargets != nil {
		var orBlock []bson.M
		for _, at := range f.AllowedTargets {
			f := bson.M{"target.type": at.Type}
			if at.Values != nil {
				f["target.value"] = bson.M{"$in": at.Values}
			}
			orBlock = append(orBlock, f)
		}
		if len(orBlock) == 0 {
			return nil, errInvalidQuery
		}
		query["$or"] = orBlock
	}
	if f.Target.Type != "" {
		query["target.type"] = f.Target.Type
	}
	if f.Target.Value != "" {
		query["target.value"] = f.Target.Value
	}
	if f.KindType != "" {
		query["kind.type"] = f.KindType
	}
	if f.KindName != "" {
		query["kind.name"] = f.KindName
	}
	if f.OwnerType != "" {
		query["owner.type"] = f.OwnerType
	}
	if f.OwnerName != "" {
		query["owner.name"] = f.OwnerName
	}
	var timeParts []bson.M
	if !f.Since.IsZero() {
		timeParts = append(timeParts, bson.M{"starttime": bson.M{"$gte": f.Since}})
	}
	if !f.Until.IsZero() {
		timeParts = append(timeParts, bson.M{"starttime": bson.M{"$lte": f.Until}})
	}
	if len(timeParts) != 0 {
		query["$and"] = timeParts
	}
	if f.Running != nil {
		query["running"] = *f.Running
	}
	if !f.IncludeRemoved {
		query["removedate"] = bson.M{"$exists": false}
	}
	if f.ErrorOnly {
		query["error"] = bson.M{"$ne": ""}
	}
	if f.Raw != nil {
		for k, v := range f.Raw {
			query[k] = v
		}
	}
	return query, nil
}

func GetKinds() ([]Kind, error) {
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	coll := conn.Events()
	var kinds []Kind
	err = coll.Find(nil).Distinct("kind", &kinds)
	if err != nil {
		return nil, err
	}
	return kinds, nil
}

func GetRunning(target Target, kind string) (*Event, error) {
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	coll := conn.Events()
	var evt Event
	err = coll.Find(bson.M{
		"_id":       eventID{Target: target},
		"kind.name": kind,
		"running":   true,
	}).One(&evt.eventData)
	if err != nil {
		if err == mgo.ErrNotFound {
			return nil, ErrEventNotFound
		}
		return nil, err
	}
	return &evt, nil
}

func GetByID(id bson.ObjectId) (*Event, error) {
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	coll := conn.Events()
	var evt Event
	err = coll.Find(bson.M{
		"uniqueid": id,
	}).One(&evt.eventData)
	if err != nil {
		if err == mgo.ErrNotFound {
			return nil, ErrEventNotFound
		}
		return nil, err
	}
	return &evt, nil
}

func All() ([]Event, error) {
	return List(nil)
}

func List(filter *Filter) ([]Event, error) {
	limit := 0
	skip := 0
	var query bson.M
	var err error
	sort := "-starttime"
	if filter != nil {
		limit = filterMaxLimit
		if filter.Limit != 0 {
			limit = filter.Limit
		}
		if filter.Sort != "" {
			sort = filter.Sort
		}
		if filter.Skip > 0 {
			skip = filter.Skip
		}
		query, err = filter.toQuery()
		if err != nil {
			if err == errInvalidQuery {
				return nil, nil
			}
			return nil, err
		}
	}
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	coll := conn.Events()
	find := coll.Find(query).Sort(sort)
	if limit > 0 {
		find = find.Limit(limit)
	}
	if skip > 0 {
		find = find.Skip(skip)
	}
	var allData []eventData
	err = find.All(&allData)
	if err != nil {
		return nil, err
	}
	evts := make([]Event, len(allData))
	for i := range evts {
		evts[i].eventData = allData[i]
	}
	return evts, nil
}

func MarkAsRemoved(target Target) error {
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	coll := conn.Events()
	now := time.Now().UTC()
	_, err = coll.UpdateAll(bson.M{
		"target":     target,
		"removedate": bson.M{"$exists": false},
	}, bson.M{"$set": bson.M{"removedate": now}})
	return err
}

func New(opts *Opts) (*Event, error) {
	if opts == nil {
		return nil, ErrNoOpts
	}
	if opts.Owner == nil && opts.RawOwner.Name == "" && opts.RawOwner.Type == "" {
		return nil, ErrNoOwner
	}
	if opts.Kind == nil {
		return nil, ErrNoKind
	}
	return newEvt(opts)
}

func NewInternal(opts *Opts) (*Event, error) {
	if opts == nil {
		return nil, ErrNoOpts
	}
	if opts.Owner != nil {
		return nil, ErrInvalidOwner
	}
	if opts.Kind != nil {
		return nil, ErrInvalidKind
	}
	if opts.InternalKind == "" {
		return nil, ErrNoInternalKind
	}
	return newEvt(opts)
}

func makeBSONRaw(in interface{}) (bson.Raw, error) {
	if in == nil {
		return bson.Raw{}, nil
	}
	var kind byte
	v := reflect.ValueOf(in)
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return bson.Raw{}, nil
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Map, reflect.Struct:
		kind = 3 // BSON "Document" kind
	case reflect.Array, reflect.Slice:
		kind = 4 // BSON "Array" kind
	default:
		return bson.Raw{}, errors.Errorf("cannot use type %T as event custom data", in)
	}
	data, err := bson.Marshal(in)
	if err != nil {
		return bson.Raw{}, err
	}
	if len(data) == 0 {
		return bson.Raw{}, errors.Errorf("invalid empty bson object for object %#v", in)
	}
	return bson.Raw{
		Kind: kind,
		Data: data,
	}, nil
}

func newEvt(opts *Opts) (*Event, error) {
	updater.start()
	if opts == nil {
		return nil, ErrNoOpts
	}
	if !opts.Target.IsValid() {
		return nil, ErrNoTarget
	}
	if opts.Allowed.Scheme == "" && len(opts.Allowed.Contexts) == 0 {
		return nil, ErrNoAllowed
	}
	if opts.Cancelable && opts.AllowedCancel.Scheme == "" && len(opts.AllowedCancel.Contexts) == 0 {
		return nil, ErrNoAllowedCancel
	}
	var k Kind
	if opts.Kind == nil {
		if opts.InternalKind == "" {
			return nil, ErrNoKind
		}
		k.Type = KindTypeInternal
		k.Name = opts.InternalKind
	} else {
		k.Type = KindTypePermission
		k.Name = opts.Kind.FullName()
	}
	var o Owner
	if opts.Owner == nil {
		if opts.RawOwner.Name != "" && opts.RawOwner.Type != "" {
			o = opts.RawOwner
		} else {
			o.Type = OwnerTypeInternal
		}
	} else if opts.Owner.IsAppToken() {
		o.Type = OwnerTypeApp
		o.Name = opts.Owner.GetAppName()
	} else {
		o.Type = OwnerTypeUser
		o.Name = opts.Owner.GetUserName()
	}
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	coll := conn.Events()
	tSpec := getThrottling(&opts.Target, &k)
	if tSpec != nil && tSpec.Max > 0 && tSpec.Time > 0 {
		query := bson.M{
			"target.type":  opts.Target.Type,
			"target.value": opts.Target.Value,
			"starttime":    bson.M{"$gt": time.Now().UTC().Add(-tSpec.Time)},
		}
		if tSpec.KindName != "" {
			query["kind.name"] = tSpec.KindName
		}
		var c int
		c, err = coll.Find(query).Count()
		if err != nil {
			return nil, err
		}
		if c >= tSpec.Max {
			return nil, ErrThrottled{Spec: tSpec, Target: opts.Target}
		}
	}
	now := time.Now().UTC()
	raw, err := makeBSONRaw(opts.CustomData)
	if err != nil {
		return nil, err
	}
	uniqID := bson.NewObjectId()
	var id eventID
	if opts.DisableLock {
		id.ObjId = uniqID
	} else {
		id.Target = opts.Target
	}
	evt := Event{eventData: eventData{
		ID:              id,
		UniqueID:        uniqID,
		Target:          opts.Target,
		StartTime:       now,
		Kind:            k,
		Owner:           o,
		StartCustomData: raw,
		LockUpdateTime:  now,
		Running:         true,
		Cancelable:      opts.Cancelable,
		Allowed:         opts.Allowed,
		AllowedCancel:   opts.AllowedCancel,
	}}
	maxRetries := 1
	for i := 0; i < maxRetries+1; i++ {
		err = coll.Insert(evt.eventData)
		if err == nil {
			err = checkIsBlocked(&evt)
			if err != nil {
				evt.Done(err)
				return nil, err
			}
			if !opts.DisableLock {
				updater.addCh <- &opts.Target
			}
			return &evt, nil
		}
		if mgo.IsDup(err) {
			if i >= maxRetries || !checkIsExpired(coll, evt.ID) {
				var existing Event
				err = coll.FindId(evt.ID).One(&existing.eventData)
				if err == mgo.ErrNotFound {
					maxRetries += 1
				}
				if err == nil {
					err = ErrEventLocked{event: &existing}
				}
			}
		} else {
			return nil, err
		}
	}
	return nil, err
}

func (e *Event) RawInsert(start, other, end interface{}) error {
	e.ID = eventID{ObjId: e.UniqueID}
	var err error
	e.StartCustomData, err = makeBSONRaw(start)
	if err != nil {
		return err
	}
	e.OtherCustomData, err = makeBSONRaw(other)
	if err != nil {
		return err
	}
	e.EndCustomData, err = makeBSONRaw(end)
	if err != nil {
		return err
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	coll := conn.Events()
	return coll.Insert(e.eventData)
}

func (e *Event) Abort() error {
	return e.done(nil, nil, true)
}

func (e *Event) Done(evtErr error) error {
	return e.done(evtErr, nil, false)
}

func (e *Event) DoneCustomData(evtErr error, customData interface{}) error {
	return e.done(evtErr, customData, false)
}

func (e *Event) SetLogWriter(w io.Writer) {
	e.logWriter = w
}

func (e *Event) SetOtherCustomData(data interface{}) error {
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	coll := conn.Events()
	return coll.UpdateId(e.ID, bson.M{
		"$set": bson.M{"othercustomdata": data},
	})
}

func (e *Event) Logf(format string, params ...interface{}) {
	log.Debugf(fmt.Sprintf("%s(%s)[%s] %s", e.Target.Type, e.Target.Value, e.Kind, format), params...)
	format += "\n"
	if e.logWriter != nil {
		fmt.Fprintf(e.logWriter, format, params...)
	}
	fmt.Fprintf(&e.logBuffer, format, params...)
}

func (e *Event) Write(data []byte) (int, error) {
	if e.logWriter != nil {
		e.logWriter.Write(data)
	}
	return e.logBuffer.Write(data)
}

func (e *Event) TryCancel(reason, owner string) error {
	if !e.Cancelable || !e.Running {
		return ErrNotCancelable
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	coll := conn.Events()
	change := mgo.Change{
		Update: bson.M{"$set": bson.M{
			"cancelinfo": cancelInfo{
				Owner:     owner,
				Reason:    reason,
				StartTime: time.Now().UTC(),
				Asked:     true,
			},
		}},
		ReturnNew: true,
	}
	_, err = coll.Find(bson.M{"_id": e.ID, "cancelinfo.asked": false}).Apply(change, &e.eventData)
	if err == mgo.ErrNotFound {
		return ErrEventNotFound
	}
	return err
}

func (e *Event) AckCancel() (bool, error) {
	if !e.Cancelable || !e.Running {
		return false, nil
	}
	conn, err := db.Conn()
	if err != nil {
		return false, err
	}
	defer conn.Close()
	coll := conn.Events()
	change := mgo.Change{
		Update: bson.M{"$set": bson.M{
			"cancelinfo.acktime":  time.Now().UTC(),
			"cancelinfo.canceled": true,
		}},
		ReturnNew: true,
	}
	_, err = coll.Find(bson.M{"_id": e.ID, "cancelinfo.asked": true}).Apply(change, &e.eventData)
	if err == mgo.ErrNotFound {
		return false, nil
	}
	return err == nil, err
}

func (e *Event) StartData(value interface{}) error {
	if e.StartCustomData.Kind == 0 {
		return nil
	}
	return e.StartCustomData.Unmarshal(value)
}

func (e *Event) EndData(value interface{}) error {
	if e.EndCustomData.Kind == 0 {
		return nil
	}
	return e.EndCustomData.Unmarshal(value)
}

func (e *Event) OtherData(value interface{}) error {
	if e.OtherCustomData.Kind == 0 {
		return nil
	}
	return e.OtherCustomData.Unmarshal(value)
}

func (e *Event) done(evtErr error, customData interface{}, abort bool) (err error) {
	// Done will be usually called in a defer block ignoring errors. This is
	// why we log error messages here.
	defer func() {
		if err != nil {
			log.Errorf("[events] error marking event as done - %#v: %s", e, err)
		}
	}()
	updater.removeCh <- &e.Target
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	coll := conn.Events()
	if abort {
		return coll.RemoveId(e.ID)
	}
	if evtErr != nil {
		e.Error = evtErr.Error()
	} else if e.CancelInfo.Canceled {
		e.Error = "canceled by user request"
	}
	e.EndTime = time.Now().UTC()
	e.EndCustomData, err = makeBSONRaw(customData)
	if err != nil {
		return err
	}
	e.Running = false
	e.Log = e.logBuffer.String()
	var dbEvt Event
	err = coll.FindId(e.ID).One(&dbEvt.eventData)
	if err == nil {
		e.OtherCustomData = dbEvt.OtherCustomData
	}
	if len(e.ID.ObjId) != 0 {
		return coll.UpdateId(e.ID, e.eventData)
	}
	defer coll.RemoveId(e.ID)
	e.ID = eventID{ObjId: e.UniqueID}
	return coll.Insert(e.eventData)
}

type lockUpdater struct {
	addCh    chan *Target
	removeCh chan *Target
	stopCh   chan struct{}
	once     *sync.Once
}

func (l *lockUpdater) start() {
	l.once.Do(func() {
		l.stopCh = make(chan struct{})
		go l.spin()
	})
}

func (l *lockUpdater) stop() {
	if l.stopCh == nil {
		return
	}
	l.stopCh <- struct{}{}
	l.stopCh = nil
	l.once = &sync.Once{}
}

func (l *lockUpdater) spin() {
	set := map[Target]struct{}{}
	for {
		select {
		case added := <-l.addCh:
			set[*added] = struct{}{}
		case removed := <-l.removeCh:
			delete(set, *removed)
		case <-l.stopCh:
			return
		case <-time.After(lockUpdateInterval):
		}
		conn, err := db.Conn()
		if err != nil {
			log.Errorf("[events] [lock update] error getting db conn: %s", err)
			continue
		}
		coll := conn.Events()
		slice := make([]interface{}, len(set))
		i := 0
		for id := range set {
			slice[i], _ = id.GetBSON()
			i++
		}
		err = coll.Update(bson.M{"_id": bson.M{"$in": slice}}, bson.M{"$set": bson.M{"lockupdatetime": time.Now().UTC()}})
		if err != nil && err != mgo.ErrNotFound {
			log.Errorf("[events] [lock update] error updating: %s", err)
		}
		conn.Close()
	}
}

func checkIsExpired(coll *storage.Collection, id interface{}) bool {
	var existingEvt Event
	err := coll.FindId(id).One(&existingEvt.eventData)
	if err == nil {
		now := time.Now().UTC()
		lastUpdate := existingEvt.LockUpdateTime.UTC()
		if now.After(lastUpdate.Add(lockExpireTimeout)) {
			existingEvt.Done(errors.Errorf("event expired, no update for %v", time.Since(lastUpdate)))
			return true
		}
	}
	return false
}

func FormToCustomData(form url.Values) []map[string]interface{} {
	ret := make([]map[string]interface{}, 0, len(form))
	for k, v := range form {
		var val interface{} = v
		if len(v) == 1 {
			val = v[0]
		}
		ret = append(ret, map[string]interface{}{"name": k, "value": val})
	}
	return ret
}

func Migrate(query bson.M, cb func(*Event) error) error {
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	coll := conn.Events()
	iter := coll.Find(query).Iter()
	var evtData eventData
	for iter.Next(&evtData) {
		evt := &Event{eventData: evtData}
		err = cb(evt)
		if err != nil {
			return errors.Wrapf(err, "unable to migrate %#v", evt)
		}
		err = coll.UpdateId(evt.ID, evt.eventData)
		if err != nil {
			return errors.Wrapf(err, "unable to update %#v", evt)
		}
	}
	return iter.Close()
}
