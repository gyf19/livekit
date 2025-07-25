// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"google.golang.org/protobuf/proto"
)

var (
	errFragmentsInHTTP    = errors.New("should not get fragments via HTTP request")
	errUnknownMessageType = errors.New("unknown message type")
)

const (
	cRTCv2Path              = "/rtc/v2"
	cRTCv2ParticipantIDPath = "/rtc/v2/{participant_id}"
)

type RTCv2Service struct {
	http.Handler

	limits        config.LimitConfig
	roomAllocator RoomAllocator
	router        routing.MessageRouter

	topicFormatter            rpc.TopicFormatter
	signalv2ParticipantClient rpc.TypedSignalv2ParticipantClient
}

func NewRTCv2Service(
	config *config.Config,
	roomAllocator RoomAllocator,
	router routing.MessageRouter,
	topicFormatter rpc.TopicFormatter,
	signalv2ParticipantClient rpc.TypedSignalv2ParticipantClient,
) *RTCv2Service {
	return &RTCv2Service{
		limits:                    config.Limit,
		router:                    router,
		roomAllocator:             roomAllocator,
		topicFormatter:            topicFormatter,
		signalv2ParticipantClient: signalv2ParticipantClient,
	}
}

func (s *RTCv2Service) SetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+cRTCv2Path, s.handlePost)
	mux.HandleFunc("PATCH "+cRTCv2ParticipantIDPath, s.handleParticipantPatch)
}

func (s *RTCv2Service) validateInternal(
	lgr logger.Logger,
	r *http.Request,
	connectRequest *livekit.ConnectRequest,
) (livekit.RoomName, livekit.ParticipantIdentity, *rpc.RelaySignalv2ConnectRequest, int, error) {
	params := ValidateConnectRequestParams{
		metadata:   connectRequest.Metadata,
		attributes: connectRequest.ParticipantAttributes,
	}

	res, code, err := ValidateConnectRequest(
		lgr,
		r,
		s.limits,
		params,
		s.router,
		s.roomAllocator,
	)
	if err != nil {
		return "", "", nil, code, err
	}

	grantsJson, err := json.Marshal(res.grants)
	if err != nil {
		return "", "", nil, http.StatusInternalServerError, err
	}

	AugmentClientInfo(connectRequest.ClientInfo, r)

	return res.roomName,
		livekit.ParticipantIdentity(res.grants.Identity),
		&rpc.RelaySignalv2ConnectRequest{
			GrantsJson:     string(grantsJson),
			CreateRoom:     res.createRoomRequest,
			ConnectRequest: connectRequest,
		},
		code,
		err
}

func (s *RTCv2Service) handlePost(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-type") != "application/x-protobuf" {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("unsupported content-type: %s", r.Header.Get("Content-type")))
		return
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("could not read request body: %w", err))
		return
	}

	wireMessage := &livekit.Signalv2WireMessage{}
	err = proto.Unmarshal(body, wireMessage)
	if err != nil {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("could not unmarshal request: %w", err))
		return
	}

	switch msg := wireMessage.GetMessage().(type) {
	case *livekit.Signalv2WireMessage_Envelope:
		for _, innerMsg := range msg.Envelope.GetClientMessages() {
			switch clientMessage := innerMsg.GetMessage().(type) {
			case *livekit.Signalv2ClientMessage_ConnectRequest:
				roomName, participantIdentity, rscr, code, err := s.validateInternal(
					logger.GetLogger(),
					r,
					clientMessage.ConnectRequest,
				)
				if err != nil {
					HandleErrorJson(w, r, code, err)
					return
				}

				if err := s.roomAllocator.SelectRoomNode(r.Context(), roomName, ""); err != nil {
					HandleErrorJson(w, r, http.StatusInternalServerError, err)
					return
				}

				resp, err := s.router.HandleParticipantConnectRequest(r.Context(), roomName, participantIdentity, rscr)
				if err != nil {
					HandleErrorJson(w, r, http.StatusInternalServerError, err)
					return
				}

				// SIGNALLING-V2-TODO: this needs to be in signal cache and get messageId
				wireMessage := &livekit.Signalv2WireMessage{
					Message: &livekit.Signalv2WireMessage_Envelope{
						Envelope: &livekit.Envelope{
							ServerMessages: []*livekit.Signalv2ServerMessage{
								&livekit.Signalv2ServerMessage{
									Message: &livekit.Signalv2ServerMessage_ConnectResponse{
										ConnectResponse: resp.ConnectResponse,
									},
								},
							},
						},
					},
				}
				marshalled, err := proto.Marshal(wireMessage)
				if err != nil {
					HandleErrorJson(w, r, http.StatusInternalServerError, err)
					return
				}

				w.Header().Add("Content-type", "application/x-protobuf")
				w.Write(marshalled)

				logger.Debugw(
					"connect response",
					"room", roomName,
					"roomID", resp.ConnectResponse.Room.Sid, // SIGNALLING-V2-TODO: roomID may not be resolved
					"participant", participantIdentity,
					"pID", resp.ConnectResponse.Participant.Sid,
					"connectResponse", logger.Proto(resp.ConnectResponse),
				)

			default:
				HandleErrorJson(
					w,
					r,
					http.StatusBadRequest,
					fmt.Errorf("%w, message: %T", errUnknownMessageType, clientMessage),
				)
			}
		}

	case *livekit.Signalv2WireMessage_Fragment:
		logger.Errorw("signalv2 bad request", errFragmentsInHTTP)
		HandleErrorJson(w, r, http.StatusBadRequest, errFragmentsInHTTP)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *RTCv2Service) handleParticipantPatch(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-type") != "application/x-protobuf" {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("unsupported content-type: %s", r.Header.Get("Content-type")))
		return
	}

	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil {
		HandleErrorJson(w, r, http.StatusUnauthorized, rtc.ErrPermissionDenied)
		return
	}

	roomName, err := EnsureJoinPermission(r.Context())
	if err != nil {
		HandleErrorJson(w, r, http.StatusUnauthorized, err)
		return
	}
	if roomName == "" {
		HandleErrorJson(w, r, http.StatusUnauthorized, ErrNoRoomName)
		return
	}

	participantIdentity := livekit.ParticipantIdentity(claims.Identity)
	if participantIdentity == "" {
		HandleErrorJson(w, r, http.StatusUnauthorized, ErrIdentityEmpty)
		return
	}

	pID := livekit.ParticipantID(r.PathValue("participant_id"))
	if pID == "" {
		HandleErrorJson(w, r, http.StatusBadRequest, ErrParticipantSidEmpty)
		return
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("could not read request body: %w", err))
		return
	}

	wireMessage := &livekit.Signalv2WireMessage{}
	err = proto.Unmarshal(body, wireMessage)
	if err != nil {
		HandleErrorJson(w, r, http.StatusBadRequest, fmt.Errorf("could not unmarshal request: %w", err))
		return
	}

	res, err := s.signalv2ParticipantClient.RelaySignalv2Participant(
		r.Context(),
		s.topicFormatter.ParticipantTopic(r.Context(), roomName, participantIdentity),
		&rpc.RelaySignalv2ParticipantRequest{
			Room:                string(roomName),
			ParticipantIdentity: string(participantIdentity),
			ParticipantId:       string(pID),
			WireMessage:         wireMessage,
		},
	)
	if err != nil {
		var pe psrpc.Error
		if errors.As(err, &pe) {
			switch pe.Code() {
			case psrpc.NotFound:
				HandleErrorJson(w, r, http.StatusNotFound, errors.New(pe.Error()))

			case psrpc.InvalidArgument:
				HandleErrorJson(w, r, http.StatusBadRequest, errors.New(pe.Error()))
			default:
				HandleErrorJson(w, r, http.StatusInternalServerError, errors.New(pe.Error()))
			}
		} else {
			HandleErrorJson(w, r, http.StatusInternalServerError, nil)
		}
		return
	}

	logger.Debugw(
		"participant response",
		"room", roomName,
		"participant", participantIdentity,
		"pID", pID,
		"participantResponse", logger.Proto(res),
	)

	marshalled, err := proto.Marshal(res.WireMessage)
	if err != nil {
		HandleErrorJson(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Add("Content-type", "application/x-protobuf")
	w.Write(marshalled)

	w.WriteHeader(http.StatusOK)
}
