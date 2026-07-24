package graph

import "testing"

func TestGraphMailboxPrincipalByAuthMode(t *testing.T) {
	tests := []struct {
		name     string
		authMode string
		userID   string
		want     string
		wantErr  bool
	}{
		{
			name:     "delegated_uses_me_even_when_user_is_configured",
			authMode: "delegated",
			userID:   "configured@example.com",
			want:     "me",
		},
		{
			name:     "app_only_uses_escaped_configured_user",
			authMode: "cert",
			userID:   "mailbox/name@example.com",
			want:     "users/mailbox%2Fname@example.com",
		},
		{
			name:     "app_only_requires_configured_user",
			authMode: "cert",
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := graphMailboxPrincipal(tc.authMode, tc.userID)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("graphMailboxPrincipal: %v", err)
			}
			if got != tc.want {
				t.Fatalf("principal = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMailboxResourcesSharePrincipal(t *testing.T) {
	tests := []struct {
		name      string
		principal string
		folderID  string
		want      string
	}{
		{
			name:      "delegated_inbox",
			principal: "me",
			folderID:  "Inbox",
			want:      "me/mailFolders('Inbox')/messages",
		},
		{
			name:      "delegated_non_inbox",
			principal: "me",
			folderID:  "unsorted-folder-id",
			want:      "me/mailFolders('unsorted-folder-id')/messages",
		},
		{
			name:      "app_only_inbox",
			principal: "users/mailbox%2Fname@example.com",
			folderID:  "Inbox",
			want:      "users/mailbox%2Fname@example.com/mailFolders('Inbox')/messages",
		},
		{
			name:      "app_only_non_inbox",
			principal: "users/mailbox%2Fname@example.com",
			folderID:  "unsorted-folder-id",
			want:      "users/mailbox%2Fname@example.com/mailFolders('unsorted-folder-id')/messages",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{baseURL: "https://graph.example/v1.0", mailboxPrincipal: tc.principal}
			if got := client.MailFolderMessagesResource(tc.folderID); got != tc.want {
				t.Fatalf("subscription resource = %q, want %q", got, tc.want)
			}
			wantURL := "https://graph.example/v1.0/" + tc.principal + "/mailFolders"
			if got := client.mailboxURL("mailFolders"); got != wantURL {
				t.Fatalf("ordinary Graph URL = %q, want %q", got, wantURL)
			}
		})
	}
}

func TestMailFolderMessagesResourceEscapesODataQuote(t *testing.T) {
	client := &Client{mailboxPrincipal: "me"}
	got := client.MailFolderMessagesResource("folder'id")
	want := "me/mailFolders('folder''id')/messages"
	if got != want {
		t.Fatalf("resource = %q, want %q", got, want)
	}
}
