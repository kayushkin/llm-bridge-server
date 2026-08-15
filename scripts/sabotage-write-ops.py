#!/usr/bin/env python3
"""Score mailstack's tests for the Gmail write ops by breaking them.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-write-ops.py [--diffs] [--crosstable]

⚠️ **This is the engine's SIXTH copy, and it is the same blob as the other
five.** md5 `9a81a32e5827b59c1a3093bf88187b17`, taken from git blob
`664f35f475edb9b7d018a28136211bf58a0ff53e` — what
`scheduler/fix/the-scorer-counts-occurrences-not-files`,
`bundle-store/docs/one-sabotage-engine-again`,
`agent-store/test/the-tracked-file-switch-is-unreached` and
`skill-store/test/the-install-path-is-unreached` all carry. Diff before editing;
a seventh blob is a fork. Take the blob off the BRANCH, not out of a working
tree — those checkouts sit on whatever branch their last pass left them on, and
md5summing them answers about the wrong commit (221st).

Why this seam is worth a scorer: measured on `main`, a `panic()` on the first
line of ANY of these thirteen left `go test ./...` green.

    backend/gmail.go  MarkRead        the census row
    backend/gmail.go  Flag            the census row
    backend/gmail.go  modify          builds every label change the two of them make
    backend/gmail.go  Move            the third writer through modify
    backend/gmail.go  Delete          trash, recoverable
    backend/gmail.go  PermDelete      not recoverable
    backend/gmail.go  do              auth header, path, and the 401 retry
    server.go         handleMarkRead  ┐
    server.go         handleFlag      │
    server.go         handleMove      ├ the five routes above the backend
    server.go         handleDelete    │
    server.go         handlePermDelete┘
    server.go         backendForReq   the account guard all five share

The census named two of the thirteen. That is the fifth consecutive row on
`e9b0b89c` where the function name under-described the mechanism it belongs to,
and the 222nd's rule — read the row's neighbours before scoping the work to its
title — has now not once failed to pay.

⚠️ Read that measurement on `main`. mailstack's checkout was already standing
there, which is the only reason it was safe to take at face value; the 223rd's
rule is that a reach guard run on whatever branch a checkout was left on answers
about the wrong base, and it fails in the direction that closes a row without
testing it.

What the mechanism does that makes it worth more than a coverage row: MarkRead
and Flag have OPPOSITE polarity. `read=true` REMOVES the UNREAD label while
`flagged=true` ADDS the STARRED one, because Gmail models read-ness as the
absence of a label and starred-ness as the presence of one. The two functions
sit four lines apart, are otherwise identical in shape, and a copy-paste between
them is invisible to any test that asserts only that a request was made.
"""

import sys

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))

from sabotage import REPO, Case, score  # noqa: E402

TARGETS = [REPO / "backend" / "gmail.go", REPO / "server.go"]
PACKAGES = ["./backend", "."]

CASES = [
    # ---- the label arithmetic: which direction "on" means ----
    Case(
        "marking a message read ADDS the unread label instead of removing it",
        [('\t\treturn g.modify(ctx, id, gmailModifyRequest{RemoveLabelIDs: []string{"UNREAD"}})',
          '\t\treturn g.modify(ctx, id, gmailModifyRequest{AddLabelIDs: []string{"UNREAD"}})')],
    ),
    Case(
        "marking a message unread REMOVES the unread label instead of adding it",
        [('\treturn g.modify(ctx, id, gmailModifyRequest{AddLabelIDs: []string{"UNREAD"}})',
          '\treturn g.modify(ctx, id, gmailModifyRequest{RemoveLabelIDs: []string{"UNREAD"}})')],
    ),
    Case(
        "flagging a message REMOVES the starred label instead of adding it",
        [('\t\treturn g.modify(ctx, id, gmailModifyRequest{AddLabelIDs: []string{"STARRED"}})',
          '\t\treturn g.modify(ctx, id, gmailModifyRequest{RemoveLabelIDs: []string{"STARRED"}})')],
    ),
    Case(
        "unflagging a message ADDS the starred label instead of removing it",
        [('\treturn g.modify(ctx, id, gmailModifyRequest{RemoveLabelIDs: []string{"STARRED"}})',
          '\treturn g.modify(ctx, id, gmailModifyRequest{AddLabelIDs: []string{"STARRED"}})')],
    ),
    Case(
        "read and flag are given the SAME polarity, which is the copy-paste this pair invites",
        [('\tif flagged {\n\t\treturn g.modify(ctx, id, gmailModifyRequest{AddLabelIDs: []string{"STARRED"}})\n\t}\n\treturn g.modify(ctx, id, gmailModifyRequest{RemoveLabelIDs: []string{"STARRED"}})',
          '\tif flagged {\n\t\treturn g.modify(ctx, id, gmailModifyRequest{RemoveLabelIDs: []string{"STARRED"}})\n\t}\n\treturn g.modify(ctx, id, gmailModifyRequest{AddLabelIDs: []string{"STARRED"}})')],
    ),

    # ---- what reaches the wire ----
    Case(
        "the unused label key is sent as null rather than omitted",
        [('\tAddLabelIDs    []string `json:"addLabelIds,omitempty"`',
          '\tAddLabelIDs    []string `json:"addLabelIds"`')],
    ),
    Case(
        "the removal key is sent as null rather than omitted",
        [('\tRemoveLabelIDs []string `json:"removeLabelIds,omitempty"`',
          '\tRemoveLabelIDs []string `json:"removeLabelIds"`')],
    ),

    # ---- Move ----
    Case(
        "moving to a folder leaves the message in the inbox as well",
        [('\tif folderID != "INBOX" {\n\t\treq.RemoveLabelIDs = []string{"INBOX"}\n\t}\n', "")],
    ),
    Case(
        "moving TO the inbox also removes the inbox, so the message lands nowhere",
        [('\tif folderID != "INBOX" {\n\t\treq.RemoveLabelIDs = []string{"INBOX"}\n\t}',
          '\treq.RemoveLabelIDs = []string{"INBOX"}')],
    ),
    Case(
        "an empty folder reaches Gmail as a modify with no labels, which answers 200 and does nothing",
        [('\tif folderID == "" {\n\t\treturn fmt.Errorf("gmail Move: folder is required")\n\t}\n', "")],
    ),

    # ---- status handling ----
    Case(
        "modify treats any status as success, so a refused label change reports ok",
        [('\tif resp.StatusCode != http.StatusOK {\n\t\tb, _ := io.ReadAll(resp.Body)\n\t\treturn fmt.Errorf("gmail modify(%s) %d: %s", id, resp.StatusCode, string(b))\n\t}\n',
          "")],
    ),
    Case(
        "the trash call treats any status as success",
        [('\tif resp.StatusCode != http.StatusOK {\n\t\tb, _ := io.ReadAll(resp.Body)\n\t\treturn fmt.Errorf("gmail trash(%s) %d: %s", id, resp.StatusCode, string(b))\n\t}\n',
          "")],
    ),
    Case(
        "permanent delete accepts only 204, so a 200 from Gmail reads as a failure",
        [("\tif resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {",
          "\tif resp.StatusCode != http.StatusNoContent {")],
    ),
    Case(
        "permanent delete accepts only 200, so a 204 from Gmail reads as a failure",
        [("\tif resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {",
          "\tif resp.StatusCode != http.StatusOK {")],
    ),

    # ---- paths and verbs ----
    Case(
        "the message id is concatenated unescaped, so an id with a separator addresses another endpoint",
        [('"/users/me/messages/"+url.PathEscape(id)+"/modify"',
          '"/users/me/messages/"+id+"/modify"')],
    ),
    Case(
        "delete goes to the permanent endpoint instead of the trash",
        [('resp, err := g.do(ctx, "POST", "/users/me/messages/"+url.PathEscape(id)+"/trash", nil)',
          'resp, err := g.do(ctx, "DELETE", "/users/me/messages/"+url.PathEscape(id), nil)')],
    ),
    Case(
        "permanent delete only trashes, so a message the user destroyed is recoverable",
        [('resp, err := g.do(ctx, "DELETE", "/users/me/messages/"+url.PathEscape(id), nil)',
          'resp, err := g.do(ctx, "POST", "/users/me/messages/"+url.PathEscape(id)+"/trash", nil)')],
    ),

    # ---- do: auth and headers ----
    Case(
        "the bearer token is not sent on the first attempt",
        [('\treq.Header.Set("Authorization", "Bearer "+token)\n\tif body != nil {\n\t\treq.Header.Set("Content-Type", "application/json")\n\t}\n\tresp, err := g.http.Do(req)',
          "\tresp, err := g.http.Do(req)")],
    ),
    Case(
        "a bodyless request still declares a JSON content type",
        [('\treq.Header.Set("Authorization", "Bearer "+token)\n\tif body != nil {\n\t\treq.Header.Set("Content-Type", "application/json")\n\t}\n\tresp, err := g.http.Do(req)',
          '\treq.Header.Set("Authorization", "Bearer "+token)\n\treq.Header.Set("Content-Type", "application/json")\n\tresp, err := g.http.Do(req)')],
    ),
    Case(
        "a missing credential is ignored and the write goes out unauthenticated",
        [("\ttoken, err := g.tokens.Token(g.apiauthProvider, g.apiauthAccount)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\treq, err := http.NewRequestWithContext(ctx, method, gmailBase+path, body)",
          "\ttoken, _ := g.tokens.Token(g.apiauthProvider, g.apiauthAccount)\n\treq, err := http.NewRequestWithContext(ctx, method, gmailBase+path, body)")],
    ),

    # ---- do: the 401 retry ----
    Case(
        "a 401 is returned to the caller instead of being retried, so a rotated token fails the write",
        [("\tif resp.StatusCode == http.StatusUnauthorized {", "\tif false {")],
    ),
    Case(
        "the stale token is not invalidated, so the retry re-sends the credential that just failed",
        [("\t\tg.tokens.Invalidate(g.apiauthProvider, g.apiauthAccount)\n", "")],
    ),

    # ---- the handlers ----
    Case(
        "a write op with no account is answered by whichever backend the empty id resolves to",
        [('\tif acct == "" {\n\t\twriteError(w, http.StatusBadRequest, errors.New("account query param required"))\n\t\treturn nil, "", false\n\t}\n',
          "")],
    ),
    Case(
        "a malformed read request is treated as mark-unread rather than rejected",
        [("\tvar body struct {\n\t\tRead bool `json:\"read\"`\n\t}\n\tif err := json.NewDecoder(r.Body).Decode(&body); err != nil {\n\t\twriteError(w, http.StatusBadRequest, err)\n\t\treturn\n\t}\n",
          "\tvar body struct {\n\t\tRead bool `json:\"read\"`\n\t}\n\t_ = json.NewDecoder(r.Body).Decode(&body)\n")],
    ),
    Case(
        "a malformed flag request is treated as unflag rather than rejected",
        [("\tvar body struct {\n\t\tFlagged bool `json:\"flagged\"`\n\t}\n\tif err := json.NewDecoder(r.Body).Decode(&body); err != nil {\n\t\twriteError(w, http.StatusBadRequest, err)\n\t\treturn\n\t}\n",
          "\tvar body struct {\n\t\tFlagged bool `json:\"flagged\"`\n\t}\n\t_ = json.NewDecoder(r.Body).Decode(&body)\n")],
    ),
    Case(
        "the move handler drops its own folder guard and relies on the backend to have one",
        [('\tif body.Folder == "" {\n\t\twriteError(w, http.StatusBadRequest, errors.New("folder is required"))\n\t\treturn\n\t}\n',
          "")],
    ),
    Case(
        "a backend refusal on the read route is reported as the caller's mistake",
        [("\tif err := b.MarkRead(r.Context(), id, body.Read); err != nil {\n\t\twriteError(w, http.StatusBadGateway, err)",
          "\tif err := b.MarkRead(r.Context(), id, body.Read); err != nil {\n\t\twriteError(w, http.StatusBadRequest, err)")],
    ),
    Case(
        "a backend refusal on the flag route is swallowed and reported as success",
        [("\tif err := b.Flag(r.Context(), id, body.Flagged); err != nil {\n\t\twriteError(w, http.StatusBadGateway, err)\n\t\treturn\n\t}",
          "\tif err := b.Flag(r.Context(), id, body.Flagged); err != nil {\n\t\t_ = err\n\t}")],
    ),
    Case(
        "a backend refusal on the move route is swallowed and reported as success",
        [("\tif err := b.Move(r.Context(), id, body.Folder); err != nil {\n\t\twriteError(w, http.StatusBadGateway, err)\n\t\treturn\n\t}",
          "\tif err := b.Move(r.Context(), id, body.Folder); err != nil {\n\t\t_ = err\n\t}")],
    ),
    Case(
        "a backend refusal on the delete route is swallowed and answered 204",
        [("\tif err := b.Delete(r.Context(), id); err != nil {\n\t\twriteError(w, http.StatusBadGateway, err)\n\t\treturn\n\t}",
          "\tif err := b.Delete(r.Context(), id); err != nil {\n\t\t_ = err\n\t}")],
    ),

    # ---- the auth middleware in front of all five ----
    Case(
        "the bearer check accepts any token that starts with the right value",
        [('\t\tif !strings.HasPrefix(h, "Bearer ") || h[7:] != s.token {',
          '\t\tif !strings.HasPrefix(h, "Bearer "+s.token) {')],
    ),
    Case(
        "the middleware always takes its no-token shortcut, so every write route is open",
        [('\t\tif s.token == "" {\n\t\t\tnext.ServeHTTP(w, r)\n\t\t\treturn\n\t\t}',
          "\t\tif true {\n\t\t\tnext.ServeHTTP(w, r)\n\t\t\treturn\n\t\t}")],
    ),
    Case(
        "an error body is written as bare text, which the UI cannot render",
        [('\twriteJSON(w, status, map[string]string{"error": err.Error()})',
          "\thttp.Error(w, err.Error(), status)")],
    ),

    # ---- controls ----
    # Known-positive: the API root every request in this mechanism is addressed
    # to. Drifts the value rather than deleting the line, per the case rules —
    # every path assertion is anchored to it, so it has to redden.
    Case(
        "CONTROL known-positive: every write op is addressed to a neighbouring API root",
        [('const gmailBase = "https://gmail.googleapis.com/gmail/v1"',
          'const gmailBase = "https://gmail.googleapis.com/gmail/v2"')],
    ),
    # Known-negative: the WORDING of the modify error. The suite asserts the
    # error carries the id, the status and the upstream body — three facts this
    # rewrite preserves — and never the sentence they are arranged into.
    Case(
        "CONTROL known-negative: the modify error's sentence is rearranged",
        [('return fmt.Errorf("gmail modify(%s) %d: %s", id, resp.StatusCode, string(b))',
          'return fmt.Errorf("gmail: modify of %s refused with %d: %s", id, resp.StatusCode, string(b))')],
        expected_unnoticed="the suite asserts the id, the status and the upstream body are all present, never the sentence they sit in",
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
