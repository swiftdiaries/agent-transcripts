package web

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/swiftdiaries/agent-transcripts/internal/analytics"
	"github.com/swiftdiaries/agent-transcripts/internal/pricing"
	"github.com/swiftdiaries/agent-transcripts/internal/review"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

type workspaceView struct {
	Title          string
	SelectedView   string
	SelectedAuthor string
	OverviewURL    string
	ActivityURL    string
	MainURL        string
	Overview       bool
	Activity       []activityView
	Stream         *streamView
	Agents         []agentNavView
	AgentUsage     []agentUsageView
	Authors        []authorOptionView
	Summary        usageLedgerView
}

type activityView struct {
	Event     eventView
	AuthorKey string
	Author    string
}

type streamView struct {
	Key         string
	Label       string
	AgentID     string
	Turns       []turnView
	Diagnostics []eventView
}

type agentNavView struct {
	Key            string
	Label          string
	AgentID        string
	AgentType      string
	Completion     session.Completion
	TokenLabel     string
	Depth          int
	URL            string
	ParentSpawnURL string
	Selected       bool
}

type agentUsageView struct {
	Label         string
	ModelLabel    string
	TokenLabel    string
	CostLabel     string
	CoverageLabel string
}

type authorOptionView struct {
	Key      string
	Label    string
	URL      string
	Selected bool
}

func buildWorkspacePage(family session.SessionFamily, title string, values url.Values, prices pricing.Catalog) (page, error) {
	if title == "" {
		title = "Transcript"
	}
	workspace := review.ProjectWorkspace(family)
	view := values.Get("view")
	if view == "" {
		view = "main"
	}
	pageView := workspaceView{
		Title: title, SelectedView: view, OverviewURL: "?view=overview", ActivityURL: "?view=activity", MainURL: "?view=main",
		Overview: view == "overview", Summary: usageLedger(analytics.Summarize([]session.SessionFamily{family}, analytics.Range{All: true}, prices)),
	}
	if view != "overview" && view != "activity" && view != "main" {
		if _, ok := workspace.Stream(view); !ok {
			return page{}, fmt.Errorf("unknown workspace view %q", view)
		}
	}
	if view == "activity" {
		author := values.Get("author")
		items := workspace.Activity
		if author != "" {
			var err error
			items, err = workspace.FilterActivity(author)
			if err != nil {
				return page{}, err
			}
		}
		pageView.SelectedAuthor = author
		for _, item := range items {
			pageView.Activity = append(pageView.Activity, activityViewFor(item))
		}
	}
	if view == "main" || (len(view) > len("agent:") && view[:len("agent:")] == "agent:") {
		stream, _ := workspace.Stream(view)
		pageView.Stream = streamViewFor(stream)
	}
	for _, stream := range workspace.Agents {
		pageView.Agents = append(pageView.Agents, agentNavigation(stream, family, view))
		pageView.AgentUsage = append(pageView.AgentUsage, agentUsage(stream, family, prices))
	}
	pageView.Authors = append(pageView.Authors, authorOptionView{Key: "", Label: "All authors", URL: "?view=activity", Selected: pageView.SelectedAuthor == ""})
	for _, author := range workspace.Authors {
		pageView.Authors = append(pageView.Authors, authorOptionView{
			Key: author.Key, Label: author.Label, URL: "?view=activity&author=" + url.QueryEscape(author.Key), Selected: author.Key == pageView.SelectedAuthor,
		})
	}
	p := legacyTranscriptFamilyPage(family, title)
	p.Workspace = pageView
	return p, nil
}

func activityViewFor(item review.ActivityItem) activityView {
	event := item.Event
	return activityView{Event: workspaceEventView(item.StreamKey, event), AuthorKey: item.Author.Key, Author: item.Author.Label}
}

func streamViewFor(stream review.AgentStream) *streamView {
	view := &streamView{Key: stream.Key, Label: stream.Label, AgentID: stream.AgentID}
	for _, turn := range stream.Node.Transcript.Turns {
		view.Turns = append(view.Turns, workspaceTurnView(stream.Key, turn))
	}
	view.Diagnostics = workspaceEventViews(stream.Key, stream.Node.Transcript.Diagnostics)
	return view
}

func workspaceTurnView(streamKey string, turn review.Turn) turnView {
	return turnView{Prompt: workspaceEventView(streamKey, turn.Prompt), Events: workspaceEventViews(streamKey, turn.Events), Diagnostics: workspaceEventViews(streamKey, turn.Diagnostics)}
}

func workspaceEventViews(streamKey string, events []session.Event) []eventView {
	views := make([]eventView, 0, len(events))
	for _, event := range events {
		views = append(views, workspaceEventView(streamKey, event))
	}
	return views
}

func workspaceEventView(streamKey string, event session.Event) eventView {
	view := eventViewFor(event)
	view.ID = streamAnchor(streamKey, event.ID)
	return view
}

func agentNavigation(stream review.AgentStream, family session.SessionFamily, selected string) agentNavView {
	view := agentNavView{Key: stream.Key, Label: stream.Label, AgentID: stream.AgentID, Depth: stream.Depth, URL: "?view=" + url.QueryEscape(stream.Key) + "#selected-agent", Selected: selected == stream.Key,
		AgentType: stream.Node.AgentType, Completion: stream.Node.Completion, TokenLabel: streamTokens(family, stream)}
	if stream.ParentKey != "" && stream.ParentToolCallID != "" {
		view.ParentSpawnURL = "?view=" + url.QueryEscape(stream.ParentKey) + "#" + streamAnchor(stream.ParentKey, stream.ParentToolCallID)
	}
	return view
}

func agentUsage(stream review.AgentStream, family session.SessionFamily, prices pricing.Catalog) agentUsageView {
	member := streamMember(family, stream)
	summary := usageLedger(analytics.Summarize([]session.SessionFamily{{Main: member}}, analytics.Range{All: true}, prices))
	models := make([]string, 0, len(summary.Raw.Models))
	for _, model := range summary.Raw.Models {
		if model.Model != "" {
			models = append(models, model.Model)
		}
	}
	modelLabel := "No model evidence"
	if len(models) > 0 {
		modelLabel = strings.Join(models, ", ")
	}
	return agentUsageView{
		Label: stream.Label, ModelLabel: modelLabel, TokenLabel: summary.TokenLabel,
		CostLabel: summary.CostLabel, CoverageLabel: summary.CoverageLabel,
	}
}

func streamTokens(family session.SessionFamily, stream review.AgentStream) string {
	member := streamMember(family, stream)
	var total int64
	for _, sample := range member.Usage {
		total += sample.Tokens.Total()
	}
	return strconv.FormatInt(total, 10) + " tokens"
}

func streamMember(family session.SessionFamily, stream review.AgentStream) session.Session {
	member := family.Main
	if stream.AgentID != "" {
		for _, child := range family.Children {
			if child.AgentID == stream.AgentID {
				member = child.Session
				break
			}
		}
	}
	return member
}

func streamAnchor(streamKey, eventID string) string {
	digest := sha256.Sum256([]byte(streamKey))
	eventDigest := sha256.Sum256([]byte(eventID))
	return "event-" + fmt.Sprintf("%x", digest[:8]) + "-" + fmt.Sprintf("%x", eventDigest[:8])
}
