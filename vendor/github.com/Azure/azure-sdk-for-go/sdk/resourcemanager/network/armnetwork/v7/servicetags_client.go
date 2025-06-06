// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License. See License.txt in the project root for license information.
// Code generated by Microsoft (R) AutoRest Code Generator. DO NOT EDIT.
// Changes may cause incorrect behavior and will be lost if the code is regenerated.

package armnetwork

import (
	"context"
	"errors"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"net/http"
	"net/url"
	"strings"
)

// ServiceTagsClient contains the methods for the ServiceTags group.
// Don't use this type directly, use NewServiceTagsClient() instead.
type ServiceTagsClient struct {
	internal       *arm.Client
	subscriptionID string
}

// NewServiceTagsClient creates a new instance of ServiceTagsClient with the specified values.
//   - subscriptionID - The subscription credentials which uniquely identify the Microsoft Azure subscription. The subscription
//     ID forms part of the URI for every service call.
//   - credential - used to authorize requests. Usually a credential from azidentity.
//   - options - pass nil to accept the default values.
func NewServiceTagsClient(subscriptionID string, credential azcore.TokenCredential, options *arm.ClientOptions) (*ServiceTagsClient, error) {
	cl, err := arm.NewClient(moduleName, moduleVersion, credential, options)
	if err != nil {
		return nil, err
	}
	client := &ServiceTagsClient{
		subscriptionID: subscriptionID,
		internal:       cl,
	}
	return client, nil
}

// List - Gets a list of service tag information resources.
// If the operation fails it returns an *azcore.ResponseError type.
//
// Generated from API version 2024-07-01
//   - location - The location that will be used as a reference for version (not as a filter based on location, you will get the
//     list of service tags with prefix details across all regions but limited to the cloud that
//     your subscription belongs to).
//   - options - ServiceTagsClientListOptions contains the optional parameters for the ServiceTagsClient.List method.
func (client *ServiceTagsClient) List(ctx context.Context, location string, options *ServiceTagsClientListOptions) (ServiceTagsClientListResponse, error) {
	var err error
	const operationName = "ServiceTagsClient.List"
	ctx = context.WithValue(ctx, runtime.CtxAPINameKey{}, operationName)
	ctx, endSpan := runtime.StartSpan(ctx, operationName, client.internal.Tracer(), nil)
	defer func() { endSpan(err) }()
	req, err := client.listCreateRequest(ctx, location, options)
	if err != nil {
		return ServiceTagsClientListResponse{}, err
	}
	httpResp, err := client.internal.Pipeline().Do(req)
	if err != nil {
		return ServiceTagsClientListResponse{}, err
	}
	if !runtime.HasStatusCode(httpResp, http.StatusOK) {
		err = runtime.NewResponseError(httpResp)
		return ServiceTagsClientListResponse{}, err
	}
	resp, err := client.listHandleResponse(httpResp)
	return resp, err
}

// listCreateRequest creates the List request.
func (client *ServiceTagsClient) listCreateRequest(ctx context.Context, location string, _ *ServiceTagsClientListOptions) (*policy.Request, error) {
	urlPath := "/subscriptions/{subscriptionId}/providers/Microsoft.Network/locations/{location}/serviceTags"
	if location == "" {
		return nil, errors.New("parameter location cannot be empty")
	}
	urlPath = strings.ReplaceAll(urlPath, "{location}", url.PathEscape(location))
	if client.subscriptionID == "" {
		return nil, errors.New("parameter client.subscriptionID cannot be empty")
	}
	urlPath = strings.ReplaceAll(urlPath, "{subscriptionId}", url.PathEscape(client.subscriptionID))
	req, err := runtime.NewRequest(ctx, http.MethodGet, runtime.JoinPaths(client.internal.Endpoint(), urlPath))
	if err != nil {
		return nil, err
	}
	reqQP := req.Raw().URL.Query()
	reqQP.Set("api-version", "2024-07-01")
	req.Raw().URL.RawQuery = reqQP.Encode()
	req.Raw().Header["Accept"] = []string{"application/json"}
	return req, nil
}

// listHandleResponse handles the List response.
func (client *ServiceTagsClient) listHandleResponse(resp *http.Response) (ServiceTagsClientListResponse, error) {
	result := ServiceTagsClientListResponse{}
	if err := runtime.UnmarshalAsJSON(resp, &result.ServiceTagsListResult); err != nil {
		return ServiceTagsClientListResponse{}, err
	}
	return result, nil
}
