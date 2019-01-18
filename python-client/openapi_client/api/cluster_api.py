# coding: utf-8

"""
    AIS

    AIS is a scalable object-storage based caching system with Amazon and Google Cloud backends.  # noqa: E501

    OpenAPI spec version: 1.1.0
    Contact: dfcdev@exchange.nvidia.com
    Generated by: https://openapi-generator.tech
"""


from __future__ import absolute_import

import re  # noqa: F401

# python 2 and python 3 compatibility library
import six

from openapi_client.api_client import ApiClient


class ClusterApi(object):
    """NOTE: This class is auto generated by OpenAPI Generator
    Ref: https://openapi-generator.tech

    Do not edit the class manually.
    """

    def __init__(self, api_client=None):
        if api_client is None:
            api_client = ApiClient()
        self.api_client = api_client

    def get(self, what, **kwargs):  # noqa: E501
        """Get cluster related details  # noqa: E501

        This method makes a synchronous HTTP request by default. To make an
        asynchronous HTTP request, please pass async_req=True
        >>> thread = api.get(what, async_req=True)
        >>> result = thread.get()

        :param async_req bool
        :param GetWhat what: Cluster details which need to be fetched (required)
        :param GetProps props: Additional properties describing the cluster details
        :return: object
                 If the method is called asynchronously,
                 returns the request thread.
        """
        kwargs['_return_http_data_only'] = True
        if kwargs.get('async_req'):
            return self.get_with_http_info(what, **kwargs)  # noqa: E501
        else:
            (data) = self.get_with_http_info(what, **kwargs)  # noqa: E501
            return data

    def get_with_http_info(self, what, **kwargs):  # noqa: E501
        """Get cluster related details  # noqa: E501

        This method makes a synchronous HTTP request by default. To make an
        asynchronous HTTP request, please pass async_req=True
        >>> thread = api.get_with_http_info(what, async_req=True)
        >>> result = thread.get()

        :param async_req bool
        :param GetWhat what: Cluster details which need to be fetched (required)
        :param GetProps props: Additional properties describing the cluster details
        :return: object
                 If the method is called asynchronously,
                 returns the request thread.
        """

        local_var_params = locals()

        all_params = ['what', 'props']  # noqa: E501
        all_params.append('async_req')
        all_params.append('_return_http_data_only')
        all_params.append('_preload_content')
        all_params.append('_request_timeout')

        for key, val in six.iteritems(local_var_params['kwargs']):
            if key not in all_params:
                raise TypeError(
                    "Got an unexpected keyword argument '%s'"
                    " to method get" % key
                )
            local_var_params[key] = val
        del local_var_params['kwargs']
        # verify the required parameter 'what' is set
        if ('what' not in local_var_params or
                local_var_params['what'] is None):
            raise ValueError("Missing the required parameter `what` when calling `get`")  # noqa: E501

        collection_formats = {}

        path_params = {}

        query_params = []
        if 'what' in local_var_params:
            query_params.append(('what', local_var_params['what']))  # noqa: E501
        if 'props' in local_var_params:
            query_params.append(('props', local_var_params['props']))  # noqa: E501

        header_params = {}

        form_params = []
        local_var_files = {}

        body_params = None
        # HTTP header `Accept`
        header_params['Accept'] = self.api_client.select_header_accept(
            ['application/json', 'text/plain'])  # noqa: E501

        # Authentication setting
        auth_settings = []  # noqa: E501

        return self.api_client.call_api(
            '/cluster/', 'GET',
            path_params,
            query_params,
            header_params,
            body=body_params,
            post_params=form_params,
            files=local_var_files,
            response_type='object',  # noqa: E501
            auth_settings=auth_settings,
            async_req=local_var_params.get('async_req'),
            _return_http_data_only=local_var_params.get('_return_http_data_only'),  # noqa: E501
            _preload_content=local_var_params.get('_preload_content', True),
            _request_timeout=local_var_params.get('_request_timeout'),
            collection_formats=collection_formats)

    def perform_operation(self, input_parameters, **kwargs):  # noqa: E501
        """Perform cluster wide operations such as setting config value, shutting down proxy/target etc.  # noqa: E501

        This method makes a synchronous HTTP request by default. To make an
        asynchronous HTTP request, please pass async_req=True
        >>> thread = api.perform_operation(input_parameters, async_req=True)
        >>> result = thread.get()

        :param async_req bool
        :param InputParameters input_parameters: (required)
        :return: None
                 If the method is called asynchronously,
                 returns the request thread.
        """
        kwargs['_return_http_data_only'] = True
        if kwargs.get('async_req'):
            return self.perform_operation_with_http_info(input_parameters, **kwargs)  # noqa: E501
        else:
            (data) = self.perform_operation_with_http_info(input_parameters, **kwargs)  # noqa: E501
            return data

    def perform_operation_with_http_info(self, input_parameters, **kwargs):  # noqa: E501
        """Perform cluster wide operations such as setting config value, shutting down proxy/target etc.  # noqa: E501

        This method makes a synchronous HTTP request by default. To make an
        asynchronous HTTP request, please pass async_req=True
        >>> thread = api.perform_operation_with_http_info(input_parameters, async_req=True)
        >>> result = thread.get()

        :param async_req bool
        :param InputParameters input_parameters: (required)
        :return: None
                 If the method is called asynchronously,
                 returns the request thread.
        """

        local_var_params = locals()

        all_params = ['input_parameters']  # noqa: E501
        all_params.append('async_req')
        all_params.append('_return_http_data_only')
        all_params.append('_preload_content')
        all_params.append('_request_timeout')

        for key, val in six.iteritems(local_var_params['kwargs']):
            if key not in all_params:
                raise TypeError(
                    "Got an unexpected keyword argument '%s'"
                    " to method perform_operation" % key
                )
            local_var_params[key] = val
        del local_var_params['kwargs']
        # verify the required parameter 'input_parameters' is set
        if ('input_parameters' not in local_var_params or
                local_var_params['input_parameters'] is None):
            raise ValueError("Missing the required parameter `input_parameters` when calling `perform_operation`")  # noqa: E501

        collection_formats = {}

        path_params = {}

        query_params = []

        header_params = {}

        form_params = []
        local_var_files = {}

        body_params = None
        if 'input_parameters' in local_var_params:
            body_params = local_var_params['input_parameters']
        # HTTP header `Accept`
        header_params['Accept'] = self.api_client.select_header_accept(
            ['text/plain'])  # noqa: E501

        # HTTP header `Content-Type`
        header_params['Content-Type'] = self.api_client.select_header_content_type(  # noqa: E501
            ['application/json'])  # noqa: E501

        # Authentication setting
        auth_settings = []  # noqa: E501

        return self.api_client.call_api(
            '/cluster/', 'PUT',
            path_params,
            query_params,
            header_params,
            body=body_params,
            post_params=form_params,
            files=local_var_files,
            response_type=None,  # noqa: E501
            auth_settings=auth_settings,
            async_req=local_var_params.get('async_req'),
            _return_http_data_only=local_var_params.get('_return_http_data_only'),  # noqa: E501
            _preload_content=local_var_params.get('_preload_content', True),
            _request_timeout=local_var_params.get('_request_timeout'),
            collection_formats=collection_formats)

    def register_target(self, snode, **kwargs):  # noqa: E501
        """Register storage target  # noqa: E501

        This method makes a synchronous HTTP request by default. To make an
        asynchronous HTTP request, please pass async_req=True
        >>> thread = api.register_target(snode, async_req=True)
        >>> result = thread.get()

        :param async_req bool
        :param Snode snode: (required)
        :return: None
                 If the method is called asynchronously,
                 returns the request thread.
        """
        kwargs['_return_http_data_only'] = True
        if kwargs.get('async_req'):
            return self.register_target_with_http_info(snode, **kwargs)  # noqa: E501
        else:
            (data) = self.register_target_with_http_info(snode, **kwargs)  # noqa: E501
            return data

    def register_target_with_http_info(self, snode, **kwargs):  # noqa: E501
        """Register storage target  # noqa: E501

        This method makes a synchronous HTTP request by default. To make an
        asynchronous HTTP request, please pass async_req=True
        >>> thread = api.register_target_with_http_info(snode, async_req=True)
        >>> result = thread.get()

        :param async_req bool
        :param Snode snode: (required)
        :return: None
                 If the method is called asynchronously,
                 returns the request thread.
        """

        local_var_params = locals()

        all_params = ['snode']  # noqa: E501
        all_params.append('async_req')
        all_params.append('_return_http_data_only')
        all_params.append('_preload_content')
        all_params.append('_request_timeout')

        for key, val in six.iteritems(local_var_params['kwargs']):
            if key not in all_params:
                raise TypeError(
                    "Got an unexpected keyword argument '%s'"
                    " to method register_target" % key
                )
            local_var_params[key] = val
        del local_var_params['kwargs']
        # verify the required parameter 'snode' is set
        if ('snode' not in local_var_params or
                local_var_params['snode'] is None):
            raise ValueError("Missing the required parameter `snode` when calling `register_target`")  # noqa: E501

        collection_formats = {}

        path_params = {}

        query_params = []

        header_params = {}

        form_params = []
        local_var_files = {}

        body_params = None
        if 'snode' in local_var_params:
            body_params = local_var_params['snode']
        # HTTP header `Accept`
        header_params['Accept'] = self.api_client.select_header_accept(
            ['text/plain'])  # noqa: E501

        # HTTP header `Content-Type`
        header_params['Content-Type'] = self.api_client.select_header_content_type(  # noqa: E501
            ['application/json'])  # noqa: E501

        # Authentication setting
        auth_settings = []  # noqa: E501

        return self.api_client.call_api(
            '/cluster/register/', 'POST',
            path_params,
            query_params,
            header_params,
            body=body_params,
            post_params=form_params,
            files=local_var_files,
            response_type=None,  # noqa: E501
            auth_settings=auth_settings,
            async_req=local_var_params.get('async_req'),
            _return_http_data_only=local_var_params.get('_return_http_data_only'),  # noqa: E501
            _preload_content=local_var_params.get('_preload_content', True),
            _request_timeout=local_var_params.get('_request_timeout'),
            collection_formats=collection_formats)

    def set_primary_proxy(self, primary_proxy_id, **kwargs):  # noqa: E501
        """Set primary proxy  # noqa: E501

        This method makes a synchronous HTTP request by default. To make an
        asynchronous HTTP request, please pass async_req=True
        >>> thread = api.set_primary_proxy(primary_proxy_id, async_req=True)
        >>> result = thread.get()

        :param async_req bool
        :param str primary_proxy_id: Bucket name (required)
        :return: None
                 If the method is called asynchronously,
                 returns the request thread.
        """
        kwargs['_return_http_data_only'] = True
        if kwargs.get('async_req'):
            return self.set_primary_proxy_with_http_info(primary_proxy_id, **kwargs)  # noqa: E501
        else:
            (data) = self.set_primary_proxy_with_http_info(primary_proxy_id, **kwargs)  # noqa: E501
            return data

    def set_primary_proxy_with_http_info(self, primary_proxy_id, **kwargs):  # noqa: E501
        """Set primary proxy  # noqa: E501

        This method makes a synchronous HTTP request by default. To make an
        asynchronous HTTP request, please pass async_req=True
        >>> thread = api.set_primary_proxy_with_http_info(primary_proxy_id, async_req=True)
        >>> result = thread.get()

        :param async_req bool
        :param str primary_proxy_id: Bucket name (required)
        :return: None
                 If the method is called asynchronously,
                 returns the request thread.
        """

        local_var_params = locals()

        all_params = ['primary_proxy_id']  # noqa: E501
        all_params.append('async_req')
        all_params.append('_return_http_data_only')
        all_params.append('_preload_content')
        all_params.append('_request_timeout')

        for key, val in six.iteritems(local_var_params['kwargs']):
            if key not in all_params:
                raise TypeError(
                    "Got an unexpected keyword argument '%s'"
                    " to method set_primary_proxy" % key
                )
            local_var_params[key] = val
        del local_var_params['kwargs']
        # verify the required parameter 'primary_proxy_id' is set
        if ('primary_proxy_id' not in local_var_params or
                local_var_params['primary_proxy_id'] is None):
            raise ValueError("Missing the required parameter `primary_proxy_id` when calling `set_primary_proxy`")  # noqa: E501

        collection_formats = {}

        path_params = {}
        if 'primary_proxy_id' in local_var_params:
            path_params['primary-proxy-id'] = local_var_params['primary_proxy_id']  # noqa: E501

        query_params = []

        header_params = {}

        form_params = []
        local_var_files = {}

        body_params = None
        # HTTP header `Accept`
        header_params['Accept'] = self.api_client.select_header_accept(
            ['text/plain'])  # noqa: E501

        # Authentication setting
        auth_settings = []  # noqa: E501

        return self.api_client.call_api(
            '/cluster/proxy/{primary-proxy-id}', 'PUT',
            path_params,
            query_params,
            header_params,
            body=body_params,
            post_params=form_params,
            files=local_var_files,
            response_type=None,  # noqa: E501
            auth_settings=auth_settings,
            async_req=local_var_params.get('async_req'),
            _return_http_data_only=local_var_params.get('_return_http_data_only'),  # noqa: E501
            _preload_content=local_var_params.get('_preload_content', True),
            _request_timeout=local_var_params.get('_request_timeout'),
            collection_formats=collection_formats)

    def unregister_target(self, daemon_id, **kwargs):  # noqa: E501
        """Unregister the storage target  # noqa: E501

        This method makes a synchronous HTTP request by default. To make an
        asynchronous HTTP request, please pass async_req=True
        >>> thread = api.unregister_target(daemon_id, async_req=True)
        >>> result = thread.get()

        :param async_req bool
        :param str daemon_id: ID of the target daemon (required)
        :return: None
                 If the method is called asynchronously,
                 returns the request thread.
        """
        kwargs['_return_http_data_only'] = True
        if kwargs.get('async_req'):
            return self.unregister_target_with_http_info(daemon_id, **kwargs)  # noqa: E501
        else:
            (data) = self.unregister_target_with_http_info(daemon_id, **kwargs)  # noqa: E501
            return data

    def unregister_target_with_http_info(self, daemon_id, **kwargs):  # noqa: E501
        """Unregister the storage target  # noqa: E501

        This method makes a synchronous HTTP request by default. To make an
        asynchronous HTTP request, please pass async_req=True
        >>> thread = api.unregister_target_with_http_info(daemon_id, async_req=True)
        >>> result = thread.get()

        :param async_req bool
        :param str daemon_id: ID of the target daemon (required)
        :return: None
                 If the method is called asynchronously,
                 returns the request thread.
        """

        local_var_params = locals()

        all_params = ['daemon_id']  # noqa: E501
        all_params.append('async_req')
        all_params.append('_return_http_data_only')
        all_params.append('_preload_content')
        all_params.append('_request_timeout')

        for key, val in six.iteritems(local_var_params['kwargs']):
            if key not in all_params:
                raise TypeError(
                    "Got an unexpected keyword argument '%s'"
                    " to method unregister_target" % key
                )
            local_var_params[key] = val
        del local_var_params['kwargs']
        # verify the required parameter 'daemon_id' is set
        if ('daemon_id' not in local_var_params or
                local_var_params['daemon_id'] is None):
            raise ValueError("Missing the required parameter `daemon_id` when calling `unregister_target`")  # noqa: E501

        collection_formats = {}

        path_params = {}
        if 'daemon_id' in local_var_params:
            path_params['daemonId'] = local_var_params['daemon_id']  # noqa: E501

        query_params = []

        header_params = {}

        form_params = []
        local_var_files = {}

        body_params = None
        # HTTP header `Accept`
        header_params['Accept'] = self.api_client.select_header_accept(
            ['text/plain'])  # noqa: E501

        # Authentication setting
        auth_settings = []  # noqa: E501

        return self.api_client.call_api(
            '/cluster/daemon/{daemonId}', 'DELETE',
            path_params,
            query_params,
            header_params,
            body=body_params,
            post_params=form_params,
            files=local_var_files,
            response_type=None,  # noqa: E501
            auth_settings=auth_settings,
            async_req=local_var_params.get('async_req'),
            _return_http_data_only=local_var_params.get('_return_http_data_only'),  # noqa: E501
            _preload_content=local_var_params.get('_preload_content', True),
            _request_timeout=local_var_params.get('_request_timeout'),
            collection_formats=collection_formats)
