package fwprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/DataDog/datadog-api-client-go/v2/api/datadog"
	"github.com/DataDog/datadog-api-client-go/v2/api/datadogV2"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestActionConnectionPlanPreservesTagsOnlySetOutsideTerraform(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	uiTags, diags := types.SetValueFrom(ctx, types.StringType, []string{"shimu:test", "test:shimu"})
	if diags.HasError() {
		t.Fatalf("building UI tags: %v", diags.Errors())
	}
	emptyTags, diags := types.SetValueFrom(ctx, types.StringType, []string{})
	if diags.HasError() {
		t.Fatalf("building empty tags: %v", diags.Errors())
	}

	tests := []struct {
		name        string
		tags        types.Set
		defaultTags map[string]string
		wantTags    []string
	}{
		{name: "no resource or provider tags keeps UI tags", tags: types.SetNull(types.StringType), wantTags: []string{"shimu:test", "test:shimu"}},
		{name: "explicit empty tags clear UI tags", tags: emptyTags, wantTags: []string{}},
		{name: "provider default tags own the tag set", tags: types.SetNull(types.StringType), defaultTags: map[string]string{"team": "workflow-automation"}, wantTags: []string{"team:workflow-automation"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &actionConnectionResource{DefaultTags: tt.defaultTags}
			var schemaResponse resource.SchemaResponse
			r.Schema(ctx, resource.SchemaRequest{}, &schemaResponse)

			stateModel := connectionResourceModel{
				connectionModel: connectionModel{ID: types.StringValue("connection-id"), Name: types.StringValue("connection"), Tags: types.SetNull(types.StringType)},
				EffectiveTags:   uiTags,
			}
			planModel := stateModel
			planModel.Tags = tt.tags
			planModel.EffectiveTags = types.SetUnknown(types.StringType)

			state := tfsdk.State{Schema: schemaResponse.Schema}
			if diags := state.Set(ctx, &stateModel); diags.HasError() {
				t.Fatalf("building state: %v", diags.Errors())
			}
			plan := tfsdk.Plan{Schema: schemaResponse.Schema}
			if diags := plan.Set(ctx, &planModel); diags.HasError() {
				t.Fatalf("building plan: %v", diags.Errors())
			}

			response := resource.ModifyPlanResponse{Plan: plan}
			r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state}, &response)
			if response.Diagnostics.HasError() {
				t.Fatalf("modifying plan: %v", response.Diagnostics.Errors())
			}

			var effectiveTags types.Set
			if diags := response.Plan.GetAttribute(ctx, path.Root("effective_tags"), &effectiveTags); diags.HasError() {
				t.Fatalf("reading effective tags: %v", diags.Errors())
			}
			var gotTags []string
			if diags := effectiveTags.ElementsAs(ctx, &gotTags, false); diags.HasError() {
				t.Fatalf("reading effective tag values: %v", diags.Errors())
			}
			slices.Sort(gotTags)
			if !slices.Equal(gotTags, tt.wantTags) {
				t.Fatalf("effective tags = %v, want %v", gotTags, tt.wantTags)
			}
		})
	}
}

func TestActionConnectionMetadataUpdatePreservesCredentials(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tags, diags := types.SetValueFrom(ctx, types.StringType, []string{"team:workflow"})
	if diags.HasError() {
		t.Fatalf("building tags: %v", diags.Errors())
	}

	tests := []struct {
		name            string
		connection      connectionModel
		changeName      bool
		unknownAWSData  bool
		wantIntegration bool
	}{
		{
			name: "HTTP tag update after credentials were entered outside Terraform",
			connection: connectionModel{
				Name: types.StringValue("HTTP connection"),
				HTTP: &httpConnectionModel{TokenAuth: &httpTokenAuthConnectionModel{
					Tokens: []*httpConnectionTokenModel{{
						Type: types.StringValue("SECRET"), Name: types.StringValue("apiKey"), Value: types.StringValue("PLACEHOLDER_FILL_IN_UI"),
					}},
				}},
			},
		},
		{
			name: "AWS tag update includes integration",
			connection: connectionModel{
				Name: types.StringValue("AWS connection"),
				AWS: &awsConnectionModel{AssumeRole: &awsAssumeRoleConnectionModel{
					AccountID: types.StringValue("123456789012"), Role: types.StringValue("role"),
					ExternalID: types.StringValue("external-id"), PrincipalID: types.StringValue("principal-id"),
				}},
			},
			unknownAWSData:  true,
			wantIntegration: true,
		},
		{
			name: "Datadog tag update",
			connection: connectionModel{
				Name: types.StringValue("Datadog connection"),
				Datadog: &datadogConnectionModel{APIKey: &datadogAPIKeyCredentialModel{
					APIKey: types.StringValue("placeholder-api-key"), AppKey: types.StringValue("placeholder-app-key"),
					Datacenter: types.StringValue("us1"),
				}},
			},
		},
		{
			name: "HTTP name update",
			connection: connectionModel{
				Name: types.StringValue("HTTP connection"),
				HTTP: &httpConnectionModel{TokenAuth: &httpTokenAuthConnectionModel{
					Tokens: []*httpConnectionTokenModel{{
						Type: types.StringValue("SECRET"), Name: types.StringValue("apiKey"), Value: types.StringValue("PLACEHOLDER_FILL_IN_UI"),
					}},
				}},
			},
			changeName: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prior := connectionResourceModel{connectionModel: tt.connection, EffectiveTags: types.SetNull(types.StringType)}
			plan := prior
			if tt.unknownAWSData {
				plan.AWS = &awsConnectionModel{AssumeRole: &awsAssumeRoleConnectionModel{
					AccountID: prior.AWS.AssumeRole.AccountID, Role: prior.AWS.AssumeRole.Role,
					ExternalID: types.StringUnknown(), PrincipalID: types.StringUnknown(),
				}}
			}
			if tt.changeName {
				plan.Name = types.StringValue("renamed HTTP connection")
			} else {
				plan.EffectiveTags = tags
			}

			request, err := connectionModelToUpdateApiRequest(ctx, plan, prior)
			if err != nil {
				t.Fatalf("building update: %v", err)
			}
			payload, err := json.Marshal(request)
			if err != nil {
				t.Fatalf("marshaling update: %v", err)
			}
			var body struct {
				Data struct {
					Attributes map[string]json.RawMessage `json:"attributes"`
				} `json:"data"`
			}
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Fatalf("parsing update: %v", err)
			}
			if _, exists := body.Data.Attributes["integration"]; exists != tt.wantIntegration {
				t.Fatalf("integration present = %t, want %t", exists, tt.wantIntegration)
			}
			if _, exists := body.Data.Attributes["tags"]; exists == tt.changeName {
				t.Fatalf("tags present = %t, want %t", exists, !tt.changeName)
			}
			if _, exists := body.Data.Attributes["name"]; !exists {
				t.Fatal("metadata PATCH omitted name")
			}
		})
	}
}

func TestActionConnectionIntegrationUpdateIncludesChangedCredentials(t *testing.T) {
	t.Parallel()

	prior := connectionResourceModel{connectionModel: connectionModel{
		Name: types.StringValue("HTTP connection"),
		HTTP: &httpConnectionModel{TokenAuth: &httpTokenAuthConnectionModel{
			Tokens: []*httpConnectionTokenModel{{
				Type: types.StringValue("SECRET"), Name: types.StringValue("apiKey"), Value: types.StringValue("old token"),
			}},
		}},
	}}
	plan := prior
	plan.HTTP = &httpConnectionModel{TokenAuth: &httpTokenAuthConnectionModel{
		Tokens: []*httpConnectionTokenModel{{
			Type: types.StringValue("SECRET"), Name: types.StringValue("apiKey"), Value: types.StringValue("new token"),
		}},
	}}

	request, err := connectionModelToUpdateApiRequest(context.Background(), plan, prior)
	if err != nil {
		t.Fatalf("building update: %v", err)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshaling update: %v", err)
	}
	var body struct {
		Data struct {
			Attributes map[string]json.RawMessage `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("parsing update: %v", err)
	}
	if _, exists := body.Data.Attributes["integration"]; !exists {
		t.Fatal("credential change did not include integration in PATCH")
	}
}

func TestActionConnectionHTTPUpdateOmitsUnchangedTokens(t *testing.T) {
	t.Parallel()

	oldAuth := &httpTokenAuthConnectionModel{
		Tokens: []*httpConnectionTokenModel{
			{Type: types.StringValue("SECRET"), Name: types.StringValue("apiKey"), Value: types.StringValue("PLACEHOLDER_FILL_IN_UI")},
			{Type: types.StringValue("SECRET"), Name: types.StringValue("appKey"), Value: types.StringValue("PLACEHOLDER_FILL_IN_UI")},
		},
		Headers: []*httpConnectionHeaderModel{{Name: types.StringValue("X-Test"), Value: types.StringValue("old")}},
		Body:    &httpConnectionBodyModel{ContentType: types.StringValue("application/json"), Content: types.StringValue(`{"foo":"{{ apiKey }}"}`)},
	}
	oldHTTP := &httpConnectionModel{BaseURL: types.StringValue("https://example.com"), TokenAuth: oldAuth}
	newHeaderAuth := *oldAuth
	newHeaderAuth.Headers = []*httpConnectionHeaderModel{{Name: types.StringValue("X-Test"), Value: types.StringValue("new")}}
	newTokenAuth := *oldAuth
	newTokenAuth.Tokens = []*httpConnectionTokenModel{
		{Type: types.StringValue("SECRET"), Name: types.StringValue("apiKey"), Value: types.StringValue("new key")},
		oldAuth.Tokens[1],
	}
	deletedTokenAuth := *oldAuth
	deletedTokenAuth.Tokens = oldAuth.Tokens[1:]

	tests := []struct {
		name            string
		plannedHTTP     *httpConnectionModel
		wantBaseURL     bool
		wantCredentials bool
		wantTokens      int
		wantDeleted     bool
	}{
		{name: "base URL", plannedHTTP: &httpConnectionModel{BaseURL: types.StringValue("https://new.example.com"), TokenAuth: oldAuth}, wantBaseURL: true, wantCredentials: true},
		{name: "header", plannedHTTP: &httpConnectionModel{BaseURL: oldHTTP.BaseURL, TokenAuth: &newHeaderAuth}, wantCredentials: true},
		{name: "one token changed", plannedHTTP: &httpConnectionModel{BaseURL: oldHTTP.BaseURL, TokenAuth: &newTokenAuth}, wantCredentials: true, wantTokens: 1},
		{name: "one token deleted", plannedHTTP: &httpConnectionModel{BaseURL: oldHTTP.BaseURL, TokenAuth: &deletedTokenAuth}, wantCredentials: true, wantTokens: 1, wantDeleted: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prior := connectionResourceModel{connectionModel: connectionModel{Name: types.StringValue("connection"), HTTP: oldHTTP}}
			plan := connectionResourceModel{connectionModel: connectionModel{Name: prior.Name, HTTP: tt.plannedHTTP}}
			request, err := connectionModelToUpdateApiRequest(context.Background(), plan, prior)
			if err != nil {
				t.Fatalf("building update: %v", err)
			}
			payload, err := json.Marshal(request)
			if err != nil {
				t.Fatalf("marshaling update: %v", err)
			}
			var body struct {
				Data struct {
					Attributes struct {
						Integration map[string]json.RawMessage `json:"integration"`
					} `json:"attributes"`
				} `json:"data"`
			}
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Fatalf("parsing update: %v", err)
			}
			integration := body.Data.Attributes.Integration
			if _, exists := integration["base_url"]; exists != tt.wantBaseURL {
				t.Fatalf("base_url present = %t, want %t", exists, tt.wantBaseURL)
			}
			credentialsJSON, exists := integration["credentials"]
			if exists != tt.wantCredentials {
				t.Fatalf("credentials present = %t, want %t", exists, tt.wantCredentials)
			}
			if !exists {
				return
			}
			var credentials map[string]json.RawMessage
			if err := json.Unmarshal(credentialsJSON, &credentials); err != nil {
				t.Fatalf("parsing credentials: %v", err)
			}
			if string(credentials["type"]) != `"HTTPTokenAuth"` {
				t.Fatal("HTTP update did not include credential type")
			}
			if _, exists := credentials["headers"]; !exists {
				t.Fatal("HTTP credentials update omitted headers")
			}
			if got, want := string(credentials["body"]), `{"content":"{\"foo\":\"{{ apiKey }}\"}","content_type":"application/json"}`; got != want {
				t.Fatalf("body = %s, want %s", got, want)
			}
			var tokens []json.RawMessage
			if raw, exists := credentials["tokens"]; exists {
				if err := json.Unmarshal(raw, &tokens); err != nil {
					t.Fatalf("parsing tokens: %v", err)
				}
			}
			if len(tokens) != tt.wantTokens {
				t.Fatalf("sent %d tokens, want %d", len(tokens), tt.wantTokens)
			}
			if len(tokens) == 1 {
				var token struct {
					Name    string `json:"name"`
					Deleted bool   `json:"deleted"`
				}
				if err := json.Unmarshal(tokens[0], &token); err != nil {
					t.Fatalf("parsing token update: %v", err)
				}
				if token.Name != "apiKey" || token.Deleted != tt.wantDeleted {
					t.Fatalf("unexpected token update: name %q, deleted %t", token.Name, token.Deleted)
				}
			}
		})
	}
}

type actionConnectionHTTPCredentialsUpdate struct {
	Tokens []struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Deleted bool   `json:"deleted"`
	} `json:"tokens"`
	Headers []struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Deleted bool   `json:"deleted"`
	} `json:"headers"`
	URLParameters []struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Deleted bool   `json:"deleted"`
	} `json:"url_parameters"`
	Body *struct {
		Content     *string `json:"content"`
		ContentType *string `json:"content_type"`
	} `json:"body"`
}

func actionConnectionHTTPUpdateCredentials(t *testing.T, plan, prior connectionResourceModel) actionConnectionHTTPCredentialsUpdate {
	t.Helper()
	request, err := connectionModelToUpdateApiRequest(context.Background(), plan, prior)
	if err != nil {
		t.Fatalf("building update: %v", err)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshaling update: %v", err)
	}
	var body struct {
		Data struct {
			Attributes struct {
				Integration *struct {
					Credentials *actionConnectionHTTPCredentialsUpdate `json:"credentials"`
				} `json:"integration"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("parsing update: %v", err)
	}
	if body.Data.Attributes.Integration == nil || body.Data.Attributes.Integration.Credentials == nil {
		t.Fatalf("HTTP change did not send credentials: %s", payload)
	}
	return *body.Data.Attributes.Integration.Credentials
}

// The API merges headers and URL parameters by name but clears an omitted body, so every HTTP credentials
// update must carry the full body, headers, and URL parameters in planned order, as provider 4.23 did.
func TestActionConnectionHTTPUpdatePreservesNonSecretFields(t *testing.T) {
	t.Parallel()

	priorAuth := httpTokenAuthConnectionModel{
		Tokens: []*httpConnectionTokenModel{
			{Type: types.StringValue("SECRET"), Name: types.StringValue("first"), Value: types.StringValue("first secret")},
			{Type: types.StringValue("SECRET"), Name: types.StringValue("second"), Value: types.StringValue("second secret")},
		},
		Headers: []*httpConnectionHeaderModel{
			{Name: types.StringValue("X-A"), Value: types.StringValue("a")},
			{Name: types.StringValue("X-B"), Value: types.StringValue("b")},
			{Name: types.StringValue("X-C"), Value: types.StringValue("c")},
		},
		URLParameters: []*httpConnectionUrlParameterModel{
			{Name: types.StringValue("first"), Value: types.StringValue("1")},
			{Name: types.StringValue("second"), Value: types.StringValue("2")},
		},
		Body: &httpConnectionBodyModel{ContentType: types.StringValue("application/json"), Content: types.StringValue(`{"foo":"{{ Secret }}"}`)},
	}
	prior := connectionResourceModel{connectionModel: connectionModel{
		Name: types.StringValue("HTTP connection"),
		HTTP: &httpConnectionModel{BaseURL: types.StringValue("https://example.com"), TokenAuth: &priorAuth},
	}}
	priorHeaders := []string{"X-A=a", "X-B=b", "X-C=c"}
	priorParameters := []string{"first=1", "second=2"}

	tests := []struct {
		name           string
		change         func(http *httpConnectionModel)
		wantTokens     []string
		wantHeaders    []string
		wantParameters []string
		wantEmptyBody  bool
	}{
		{
			name: "token rotation",
			change: func(http *httpConnectionModel) {
				http.TokenAuth.Tokens[0] = &httpConnectionTokenModel{Type: types.StringValue("SECRET"), Name: types.StringValue("first"), Value: types.StringValue("rotated")}
			},
			wantTokens:     []string{"first"},
			wantHeaders:    priorHeaders,
			wantParameters: priorParameters,
		},
		{
			name:           "base URL",
			change:         func(http *httpConnectionModel) { http.BaseURL = types.StringValue("https://new.example.com") },
			wantHeaders:    priorHeaders,
			wantParameters: priorParameters,
		},
		{
			name: "middle header value",
			change: func(http *httpConnectionModel) {
				http.TokenAuth.Headers[1] = &httpConnectionHeaderModel{Name: types.StringValue("X-B"), Value: types.StringValue("new b")}
			},
			wantHeaders:    []string{"X-A=a", "X-B=new b", "X-C=c"},
			wantParameters: priorParameters,
		},
		{
			name: "appended header",
			change: func(http *httpConnectionModel) {
				http.TokenAuth.Headers = append(http.TokenAuth.Headers, &httpConnectionHeaderModel{Name: types.StringValue("X-D"), Value: types.StringValue("d")})
			},
			wantHeaders:    []string{"X-A=a", "X-B=b", "X-C=c", "X-D=d"},
			wantParameters: priorParameters,
		},
		{
			name:           "removed header",
			change:         func(http *httpConnectionModel) { http.TokenAuth.Headers = http.TokenAuth.Headers[1:] },
			wantHeaders:    []string{"X-A deleted", "X-B=b", "X-C=c"},
			wantParameters: priorParameters,
		},
		{
			name: "URL parameter value",
			change: func(http *httpConnectionModel) {
				http.TokenAuth.URLParameters[1] = &httpConnectionUrlParameterModel{Name: types.StringValue("second"), Value: types.StringValue("two")}
			},
			wantHeaders:    priorHeaders,
			wantParameters: []string{"first=1", "second=two"},
		},
		{
			name:           "removed body",
			change:         func(http *httpConnectionModel) { http.TokenAuth.Body = nil },
			wantHeaders:    priorHeaders,
			wantParameters: priorParameters,
			wantEmptyBody:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plannedAuth := priorAuth
			plannedAuth.Tokens = slices.Clone(priorAuth.Tokens)
			plannedAuth.Headers = slices.Clone(priorAuth.Headers)
			plannedAuth.URLParameters = slices.Clone(priorAuth.URLParameters)
			plannedHTTP := *prior.HTTP
			plannedHTTP.TokenAuth = &plannedAuth
			tt.change(&plannedHTTP)
			plan := prior
			plan.HTTP = &plannedHTTP

			credentials := actionConnectionHTTPUpdateCredentials(t, plan, prior)

			var tokens []string
			for _, token := range credentials.Tokens {
				tokens = append(tokens, token.Name)
			}
			if !slices.Equal(tokens, tt.wantTokens) {
				t.Fatalf("tokens = %v, want %v", tokens, tt.wantTokens)
			}

			var headers []string
			for _, header := range credentials.Headers {
				if header.Deleted {
					headers = append(headers, header.Name+" deleted")
				} else {
					headers = append(headers, header.Name+"="+header.Value)
				}
			}
			if !slices.Equal(headers, tt.wantHeaders) {
				t.Fatalf("headers = %v, want %v", headers, tt.wantHeaders)
			}

			var parameters []string
			for _, parameter := range credentials.URLParameters {
				parameters = append(parameters, parameter.Name+"="+parameter.Value)
			}
			if !slices.Equal(parameters, tt.wantParameters) {
				t.Fatalf("URL parameters = %v, want %v", parameters, tt.wantParameters)
			}

			if credentials.Body == nil {
				t.Fatal("HTTP credentials update omitted body, which makes the API clear it")
			}
			if tt.wantEmptyBody {
				if credentials.Body.Content != nil || credentials.Body.ContentType != nil {
					t.Fatalf("removed body sent content %v and content type %v, want an empty body", credentials.Body.Content, credentials.Body.ContentType)
				}
				return
			}
			if credentials.Body.Content == nil || *credentials.Body.Content != `{"foo":"{{ Secret }}"}` {
				t.Fatalf("body content = %v, want the stored body", credentials.Body.Content)
			}
			if credentials.Body.ContentType == nil || *credentials.Body.ContentType != "application/json" {
				t.Fatalf("body content type = %v, want application/json", credentials.Body.ContentType)
			}
		})
	}
}

// The API appends updated tokens after unchanged ones, so a token-only update reorders the stored tokens.
func TestActionConnectionReadKeepsTokenOrderFromState(t *testing.T) {
	t.Parallel()

	const connectionID = "4f2bb3f6-0d0c-4b45-8b3c-1b3c2e0f6a11"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v2/actions/connections/"+connectionID {
			t.Errorf("unexpected request path: %s", request.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"` + connectionID + `","type":"action_connection","attributes":{"name":"HTTP connection","integration":{"type":"HTTP","base_url":"https://example.com","credentials":{"type":"HTTPTokenAuth","tokens":[{"name":"second","type":"SECRET","value":""},{"name":"new","type":"SECRET","value":""},{"name":"first","type":"SECRET","value":""}],"headers":[{"name":"X-A","value":"a"}],"body":{"content":"{\"foo\":\"bar\"}","content_type":"application/json"}}}}}}`))
	}))
	defer server.Close()

	config := datadog.NewConfiguration()
	config.Servers = datadog.ServerConfigurations{{URL: server.URL}}
	config.OperationServers = nil
	config.HTTPClient = server.Client()
	api := datadogV2.NewActionConnectionApi(datadog.NewAPIClient(config))

	state := connectionResourceModel{connectionModel: connectionModel{
		ID:   types.StringValue(connectionID),
		Name: types.StringValue("HTTP connection"),
		HTTP: &httpConnectionModel{BaseURL: types.StringValue("https://example.com"), TokenAuth: &httpTokenAuthConnectionModel{
			Tokens: []*httpConnectionTokenModel{
				{Type: types.StringValue("SECRET"), Name: types.StringValue("first"), Value: types.StringValue("first secret")},
				{Type: types.StringValue("SECRET"), Name: types.StringValue("second"), Value: types.StringValue("second secret")},
			},
		}},
	}}

	connection, _, err := readConnection(context.Background(), api, connectionID, state)
	if err != nil {
		t.Fatalf("reading connection: %v", err)
	}

	var got [][2]string
	for _, token := range connection.HTTP.TokenAuth.Tokens {
		got = append(got, [2]string{token.Name.ValueString(), token.Value.ValueString()})
	}
	want := [][2]string{{"first", "first secret"}, {"second", "second secret"}, {"new", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	if connection.HTTP.TokenAuth.Body == nil || connection.HTTP.TokenAuth.Body.Content.ValueString() != `{"foo":"bar"}` {
		t.Fatalf("body = %+v, want the API body", connection.HTTP.TokenAuth.Body)
	}
}
