package op

import (
	"errors"
	"fmt"
	"hash/crc32"
	"sync/atomic"

	"github.com/gorilla/websocket"
	"github.com/synctv-org/synctv/internal/db"
	"github.com/synctv-org/synctv/internal/model"
	"github.com/synctv-org/synctv/internal/settings"
	"github.com/synctv-org/synctv/utils"
	"github.com/zijiren233/gencontainer/rwmap"
	rtmps "github.com/zijiren233/livelib/server"
	"github.com/zijiren233/stream"
	"golang.org/x/crypto/bcrypt"
)

type Room struct {
	model.Room
	version  uint32
	current  *current
	initOnce utils.Once
	hub      *Hub
	movies   movies
	members  rwmap.RWMap[string, *model.RoomMember]
}

func (r *Room) lazyInitHub() {
	r.initOnce.Do(func() {
		r.hub = newHub(r.ID)
	})
}

func (r *Room) PeopleNum() int64 {
	if r.hub == nil {
		return 0
	}
	return r.hub.PeopleNum()
}

func (r *Room) Broadcast(data Message, conf ...BroadcastConf) error {
	if r.hub == nil {
		return nil
	}
	return r.hub.Broadcast(data, conf...)
}

func (r *Room) SendToUser(user *User, data Message) error {
	if r.hub == nil {
		return nil
	}
	return r.hub.SendToUser(user.ID, data)
}

func (r *Room) GetChannel(channelName string) (*rtmps.Channel, error) {
	return r.movies.GetChannel(channelName)
}

func (r *Room) close() {
	if r.initOnce.Done() {
		r.hub.Close()
		r.movies.Close()
	}
}

func (r *Room) Version() uint32 {
	return atomic.LoadUint32(&r.version)
}

func (r *Room) CheckVersion(version uint32) bool {
	return atomic.LoadUint32(&r.version) == version
}

func (r *Room) UpdateMovie(movieId string, movie *model.BaseMovie) error {
	if r.current.current.MovieID == movieId {
		return errors.New("cannot update current movie")
	}
	return r.movies.Update(movieId, movie)
}

func (r *Room) AddMovie(m *model.Movie) error {
	m.RoomID = r.ID
	return r.movies.AddMovie(m)
}

func (r *Room) AddMovies(movies []*model.Movie) error {
	for _, m := range movies {
		m.RoomID = r.ID
	}
	return r.movies.AddMovies(movies)
}

func (r *Room) UserRole(userID string) (model.RoomMemberRole, error) {
	if r.IsCreator(userID) {
		return model.RoomMemberRoleCreator, nil
	}
	rur, err := r.LoadRoomMember(userID)
	if err != nil {
		return model.RoomMemberRoleUnknown, err
	}
	return rur.Role, nil
}

// do not use this value for permission determination
func (r *Room) IsAdmin(userID string) bool {
	role, err := r.UserRole(userID)
	if err != nil {
		return false
	}
	return role.IsAdmin()
}

func (r *Room) IsCreator(userID string) bool {
	return r.CreatorID == userID
}

func (r *Room) IsGuest(userID string) bool {
	return userID == db.GuestUserID
}

func (r *Room) HasPermission(userID string, permission model.RoomMemberPermission) bool {
	if r.IsCreator(userID) {
		return true
	}

	rur, err := r.LoadOrCreateRoomMember(userID)
	if err != nil {
		return false
	}

	if rur.Role.IsAdmin() {
		return true
	}

	switch {
	case permission.Has(model.PermissionGetMovieList) && !r.Settings.CanGetMovieList,
		permission.Has(model.PermissionAddMovie) && !r.Settings.CanAddMovie,
		permission.Has(model.PermissionDeleteMovie) && !r.Settings.CanDeleteMovie,
		permission.Has(model.PermissionEditMovie) && !r.Settings.CanEditMovie,
		permission.Has(model.PermissionSetCurrentMovie) && !r.Settings.CanSetCurrentMovie,
		permission.Has(model.PermissionSetCurrentStatus) && !r.Settings.CanSetCurrentStatus,
		permission.Has(model.PermissionSendChatMessage) && !r.Settings.CanSendChatMessage:
		return false
	default:
		return rur.Permissions.Has(permission)
	}
}

func (r *Room) HasAdminPermission(userID string, permission model.RoomAdminPermission) bool {
	if r.CreatorID == userID {
		return true
	}

	rur, err := r.LoadOrCreateRoomMember(userID)
	if err != nil {
		return false
	}

	return rur.HasAdminPermission(permission)
}

func (r *Room) LoadOrCreateMemberStatus(userID string) (model.RoomMemberStatus, error) {
	if r.IsCreator(userID) {
		return model.RoomMemberStatusActive, nil
	}
	rur, err := r.LoadOrCreateRoomMember(userID)
	if err != nil {
		return model.RoomMemberStatusUnknown, err
	}
	return rur.Status, nil
}

func (r *Room) LoadMemberStatus(userID string) (model.RoomMemberStatus, error) {
	if r.IsCreator(userID) {
		return model.RoomMemberStatusActive, nil
	}
	rur, err := r.LoadRoomMember(userID)
	if err != nil {
		return model.RoomMemberStatusUnknown, err
	}
	return rur.Status, nil
}

func (r *Room) LoadOrCreateRoomMember(userID string) (*model.RoomMember, error) {
	if r.Settings.DisableJoinNewUser {
		return r.LoadRoomMember(userID)
	}
	if r.IsGuest(userID) && (r.Settings.DisableGuest || !settings.EnableGuest.Get()) {
		return nil, errors.New("guest is disabled")
	}
	member, ok := r.members.Load(userID)
	if ok {
		return member, nil
	}
	var conf []db.CreateRoomMemberRelationConfig
	if r.IsCreator(userID) {
		conf = append(
			conf,
			db.WithRoomMemberStatus(model.RoomMemberStatusActive),
			db.WithRoomMemberRelationPermissions(model.AllPermissions),
			db.WithRoomMemberRole(model.RoomMemberRoleCreator),
			db.WithRoomMemberAdminPermissions(model.AllAdminPermissions),
		)
	} else if r.IsGuest(userID) {
		conf = append(
			conf,
			db.WithRoomMemberStatus(model.RoomMemberStatusActive),
			db.WithRoomMemberRelationPermissions(model.NoPermission),
			db.WithRoomMemberRole(model.RoomMemberRoleMember),
			db.WithRoomMemberAdminPermissions(model.NoAdminPermission),
		)
	} else {
		conf = append(
			conf,
			db.WithRoomMemberRelationPermissions(r.Settings.UserDefaultPermissions),
		)
		if r.Settings.JoinNeedReview {
			conf = append(conf, db.WithRoomMemberStatus(model.RoomMemberStatusPending))
		} else {
			conf = append(conf, db.WithRoomMemberStatus(model.RoomMemberStatusActive))
		}
	}
	member, err := db.FirstOrCreateRoomMemberRelation(r.ID, userID, conf...)
	if err != nil {
		return nil, err
	}
	if r.IsCreator(userID) {
		member.Role = model.RoomMemberRoleCreator
		member.Permissions = model.AllPermissions
		member.AdminPermissions = model.AllAdminPermissions
		member.Status = model.RoomMemberStatusActive
	} else if r.IsGuest(userID) {
		member.Role = model.RoomMemberRoleMember
		member.Permissions = model.NoPermission
		member.AdminPermissions = model.NoAdminPermission
		member.Status = model.RoomMemberStatusActive
	} else if member.Role.IsAdmin() {
		member.Permissions = model.AllPermissions
	}
	member, _ = r.members.LoadOrStore(userID, member)
	return member, nil
}

func (r *Room) LoadRoomMember(userID string) (*model.RoomMember, error) {
	if r.IsGuest(userID) && (r.Settings.DisableGuest || !settings.EnableGuest.Get()) {
		return nil, errors.New("guest is disabled")
	}
	member, ok := r.members.Load(userID)
	if ok {
		return member, nil
	}
	member, err := db.GetRoomMember(r.ID, userID)
	if err != nil {
		return nil, fmt.Errorf("get room member failed: %w", err)
	}
	if r.IsCreator(userID) {
		member.Role = model.RoomMemberRoleCreator
		member.Permissions = model.AllPermissions
		member.AdminPermissions = model.AllAdminPermissions
		member.Status = model.RoomMemberStatusActive
	} else if r.IsGuest(userID) {
		member.Role = model.RoomMemberRoleMember
		member.Permissions = model.NoPermission
		member.AdminPermissions = model.NoAdminPermission
		member.Status = model.RoomMemberStatusActive
	} else if member.Role.IsAdmin() {
		member.Permissions = model.AllPermissions
	}
	member, _ = r.members.LoadOrStore(userID, member)
	return member, nil
}

func (r *Room) LoadRoomMemberPermission(userID string) (model.RoomMemberPermission, error) {
	if r.IsCreator(userID) {
		return model.AllPermissions, nil
	}
	member, err := r.LoadRoomMember(userID)
	if err != nil {
		return model.NoPermission, err
	}
	return member.Permissions, nil
}

func (r *Room) LoadRoomAdminPermission(userID string) (model.RoomAdminPermission, error) {
	if r.IsCreator(userID) {
		return model.AllAdminPermissions, nil
	}
	member, err := r.LoadRoomMember(userID)
	if err != nil {
		return model.NoAdminPermission, err
	}
	return member.AdminPermissions, nil
}

func (r *Room) NeedPassword() bool {
	return len(r.HashedPassword) != 0
}

func (r *Room) SetPassword(password string) error {
	if r.CheckPassword(password) && r.NeedPassword() {
		return errors.New("password is the same")
	}
	var hashedPassword []byte
	if password != "" {
		var err error
		hashedPassword, err = bcrypt.GenerateFromPassword(stream.StringToBytes(password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		atomic.StoreUint32(&r.version, crc32.ChecksumIEEE(hashedPassword))
	}
	r.HashedPassword = hashedPassword
	return db.SetRoomHashedPassword(r.ID, hashedPassword)
}

func (r *Room) GetMoviesCount() int {
	return r.movies.Len()
}

func (r *Room) DeleteMovieByID(id string) error {
	if r.current.current.MovieID == id {
		return errors.New("cannot delete current movie")
	}
	return r.movies.DeleteMovieByID(id)
}

func (r *Room) DeleteMoviesByID(ids []string) error {
	if r.current.current.MovieID != "" {
		for _, id := range ids {
			if id == r.current.current.MovieID {
				return errors.New("cannot delete current movie")
			}
		}
	}
	return r.movies.DeleteMoviesByID(ids)
}

func (r *Room) ClearMovies() error {
	_ = r.SetCurrentMovie("", false)
	return r.movies.Clear()
}

func (r *Room) GetMovieByID(id string) (*Movie, error) {
	return r.movies.GetMovieByID(id)
}

func (r *Room) Current() *Current {
	c := r.current.Current()
	return &c
}

func (r *Room) CurrentMovieID() string {
	return r.current.current.MovieID
}

var ErrNoCurrentMovie = errors.New("no current movie")

func (r *Room) CurrentMovie() (*Movie, error) {
	if r.current.current.MovieID == "" {
		return nil, ErrNoCurrentMovie
	}
	return r.GetMovieByID(r.current.current.MovieID)
}

func (r *Room) CheckCurrentExpired(expireId uint64) (bool, error) {
	m, err := r.CurrentMovie()
	if err != nil {
		return false, err
	}
	return m.CheckExpired(expireId), nil
}

func (r *Room) SetCurrentMovie(movieID string, play bool) error {
	if movieID == "" {
		r.current.SetMovie("", false, play)
		return nil
	}
	m, err := r.GetMovieByID(movieID)
	if err != nil {
		return err
	}
	r.current.SetMovie(m.ID, m.Base.Live, play)
	return nil
}

func (r *Room) SwapMoviePositions(id1, id2 string) error {
	return r.movies.SwapMoviePositions(id1, id2)
}

func (r *Room) GetMoviesWithPage(page, pageSize int, creator string) ([]*Movie, int) {
	return r.movies.GetMoviesWithPage(page, pageSize, creator)
}

func (r *Room) NewClient(user *User, conn *websocket.Conn) (*Client, error) {
	r.lazyInitHub()
	cli := newClient(user, r, conn)
	err := r.hub.RegClient(cli)
	if err != nil {
		return nil, err
	}
	return cli, nil
}

func (r *Room) RegClient(cli *Client) error {
	r.lazyInitHub()
	return r.hub.RegClient(cli)
}

func (r *Room) UnregisterClient(cli *Client) error {
	r.lazyInitHub()
	return r.hub.UnRegClient(cli)
}

func (r *Room) IsOnline(userID string) bool {
	r.lazyInitHub()
	return r.hub.IsOnline(userID)
}

func (r *Room) SetCurrentStatus(playing bool, seek float64, rate float64, timeDiff float64) *Status {
	return r.current.SetStatus(playing, seek, rate, timeDiff)
}

func (r *Room) SetCurrentSeekRate(seek float64, rate float64, timeDiff float64) *Status {
	return r.current.SetSeekRate(seek, rate, timeDiff)
}

func (r *Room) SetSettings(settings *model.RoomSettings) error {
	err := db.SaveRoomSettings(r.ID, settings)
	if err != nil {
		return err
	}
	r.Settings = settings
	return nil
}

func (r *Room) UpdateSettings(settings map[string]any) error {
	rs, err := db.UpdateRoomSettings(r.ID, settings)
	if err != nil {
		return err
	}
	r.Settings = rs
	return nil
}

func (r *Room) ResetMemberPermissions(userID string) error {
	return r.SetMemberPermissions(userID, r.Settings.UserDefaultPermissions)
}

func (r *Room) SetMemberPermissions(userID string, permissions model.RoomMemberPermission) error {
	defer r.members.Delete(userID)
	return db.SetMemberPermissions(r.ID, userID, permissions)
}

func (r *Room) AddMemberPermissions(userID string, permissions model.RoomMemberPermission) error {
	defer r.members.Delete(userID)
	return db.AddMemberPermissions(r.ID, userID, permissions)
}

func (r *Room) RemoveMemberPermissions(userID string, permissions model.RoomMemberPermission) error {
	defer r.members.Delete(userID)
	return db.RemoveMemberPermissions(r.ID, userID, permissions)
}

func (r *Room) ApprovePendingMember(userID string) error {
	defer r.members.Delete(userID)
	return db.RoomApprovePendingMember(r.ID, userID)
}

func (r *Room) BanMember(userID string) error {
	if r.IsCreator(userID) {
		return errors.New("you are creator, cannot ban")
	}
	defer func() {
		r.members.Delete(userID)
		_ = r.hub.KickUser(userID)
	}()
	return db.RoomBanMember(r.ID, userID)
}

func (r *Room) UnbanMember(userID string) error {
	if r.IsCreator(userID) {
		return errors.New("you are creator, cannot unban")
	}
	defer r.members.Delete(userID)
	return db.RoomUnbanMember(r.ID, userID)
}

func (r *Room) ResetAdminPermissions(userID string) error {
	return r.SetAdminPermissions(userID, model.DefaultAdminPermissions)
}

func (r *Room) SetAdminPermissions(userID string, permissions model.RoomAdminPermission) error {
	defer r.members.Delete(userID)
	return db.RoomSetAdminPermissions(r.ID, userID, permissions)
}

func (r *Room) AddAdminPermissions(userID string, permissions model.RoomAdminPermission) error {
	defer r.members.Delete(userID)
	return db.RoomSetAdminPermissions(r.ID, userID, permissions)
}

func (r *Room) RemoveAdminPermissions(userID string, permissions model.RoomAdminPermission) error {
	defer r.members.Delete(userID)
	return db.RoomSetAdminPermissions(r.ID, userID, 0)
}

func (r *Room) SetAdmin(userID string, permissions model.RoomAdminPermission) error {
	if r.IsCreator(userID) {
		return errors.New("you are creator, cannot set admin")
	}
	defer r.members.Delete(userID)
	return db.RoomSetAdmin(r.ID, userID, permissions)
}

func (r *Room) SetMember(userID string, permissions model.RoomMemberPermission) error {
	if r.IsCreator(userID) {
		return errors.New("you are creator, cannot set member")
	}
	defer r.members.Delete(userID)
	return db.RoomSetMember(r.ID, userID, permissions)
}
