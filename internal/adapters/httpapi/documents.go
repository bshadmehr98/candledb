package httpapi

import (
	"fmt"
	"net/http"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	path, err := domain.ParseDocPath(r.PathValue("path"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	doc, err := s.docs.Get(r.Context(), principal(r.Context()), path)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDocumentJSON(doc))
}

// handleSet is PUT /v1/documents/{path}: replace the document with the
// request body, or merge the body into it with ?merge=true.
func (s *Server) handleSet(w http.ResponseWriter, r *http.Request) {
	s.writeOne(w, r, writeRequest{Op: "set", Merge: r.URL.Query().Get("merge") == "true"})
}

// handleUpdate is PATCH /v1/documents/{path}: change the listed fields of
// an existing document. Keys are dotted field paths.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	s.writeOne(w, r, writeRequest{Op: "update"})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	s.commit(w, r, []writeRequest{{Op: "delete", Path: r.PathValue("path")}}, commitJSON{})
}

// handleAdd is POST /v1/documents/{collection}: create a document with an
// ID chosen by the server.
func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	collection, err := domain.ParseCollectionPath(r.PathValue("path"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	path, err := collection.Doc(s.docs.NewID())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	wr := writeRequest{Op: "create", Path: string(path)}
	if err := decodeBody(r, &wr.Data); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.commit(w, r, []writeRequest{wr}, commitJSON{Path: string(path), ID: path.ID()})
}

// writeOne handles the single-document endpoints whose body is the data.
func (s *Server) writeOne(w http.ResponseWriter, r *http.Request, wr writeRequest) {
	wr.Path = r.PathValue("path")
	if err := decodeBody(r, &wr.Data); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.commit(w, r, []writeRequest{wr}, commitJSON{})
}

// handleCommit is POST /v1/commit: apply a batch of writes atomically.
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	var req commitRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.commit(w, r, req.Writes, commitJSON{})
}

// commit is the one place every write endpoint ends up: translate, call the
// use case, report the result.
func (s *Server) commit(w http.ResponseWriter, r *http.Request, requests []writeRequest, response commitJSON) {
	writes := make([]domain.Write, 0, len(requests))
	for i, wr := range requests {
		write, err := wr.toDomain()
		if err != nil {
			if len(requests) > 1 {
				err = fmt.Errorf("write %d: %w", i, err) // in a batch, say which write
			}
			s.writeError(w, r, err)
			return
		}
		writes = append(writes, write)
	}
	result, err := s.docs.Commit(r.Context(), principal(r.Context()), writes)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	response.Version = result.Version
	response.CommitTime = formatTime(result.Time)
	writeJSON(w, http.StatusOK, response)
}

// handleQuery is POST /v1/query. It is a POST although it only reads,
// because a query is structured data that fits a JSON body far better than
// a URL.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req queryJSON
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	q, err := req.toDomain()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	docs, err := s.docs.Query(r.Context(), principal(r.Context()), q)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]documentJSON, len(docs))
	for i, doc := range docs {
		out[i] = toDocumentJSON(doc)
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": out})
}

// ---- accounts ----

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type userJSON struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	CreatedAt string `json:"createdAt"`
}

type sessionJSON struct {
	User         userJSON `json:"user"`
	AccessToken  string   `json:"accessToken"`
	RefreshToken string   `json:"refreshToken"`
	ExpiresAt    string   `json:"expiresAt"`
}

func toUserJSON(u app.User) userJSON {
	return userJSON{ID: u.ID, Email: u.Email, CreatedAt: formatTime(u.CreatedAt)}
}

func (s *Server) writeSession(w http.ResponseWriter, r *http.Request, sess app.AuthSession, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// Tokens must never be stored by a cache between client and server.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, sessionJSON{
		User:         toUserJSON(sess.User),
		AccessToken:  sess.AccessToken,
		RefreshToken: sess.RefreshToken,
		ExpiresAt:    formatTime(sess.ExpiresAt),
	})
}

func (s *Server) handleSignUp(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := decodeBody(r, &c); err != nil {
		s.writeError(w, r, err)
		return
	}
	sess, err := s.auth.SignUp(r.Context(), c.Email, c.Password)
	s.writeSession(w, r, sess, err)
}

func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := decodeBody(r, &c); err != nil {
		s.writeError(w, r, err)
		return
	}
	sess, err := s.auth.SignIn(r.Context(), c.Email, c.Password)
	s.writeSession(w, r, sess, err)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	sess, err := s.auth.Refresh(r.Context(), req.RefreshToken)
	s.writeSession(w, r, sess, err)
}

func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.auth.SignOut(r.Context(), req.RefreshToken); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.auth.User(r.Context(), principal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toUserJSON(user))
}
