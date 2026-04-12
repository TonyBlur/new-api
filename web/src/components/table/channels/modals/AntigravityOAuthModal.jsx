/*
Copyright (C) 2025 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

import React, { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Modal,
  Button,
  Space,
  Typography,
  Input,
  Banner,
} from '@douyinfe/semi-ui';
import { API, copy, showError, showSuccess } from '../../../../helpers';

const { Text } = Typography;

const AntigravityOAuthModal = ({ visible, onCancel, onSuccess }) => {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(false);
  const [authorizeUrl, setAuthorizeUrl] = useState('');
  const [input, setInput] = useState('');

  const startOAuth = async () => {
    setLoading(true);
    try {
      const res = await API.post(
        '/api/channel/antigravity/oauth/start',
        { channel_id: 0 },
        { skipErrorHandler: true },
      );
      if (!res?.data?.success) {
        console.error('Antigravity OAuth start failed:', res?.data?.message);
        throw new Error(t('启动授权失败'));
      }
      const url = res?.data?.data?.authorize_url || '';
      if (!url) {
        console.error(
          'Antigravity OAuth start response missing authorize_url:',
          res?.data,
        );
        throw new Error(t('响应缺少授权链接'));
      }
      setAuthorizeUrl(url);
      window.open(url, '_blank', 'noopener,noreferrer');
      showSuccess(t('已打开授权页面'));
    } catch (error) {
      showError(error?.message || t('启动授权失败'));
    } finally {
      setLoading(false);
    }
  };

  const completeOAuth = async () => {
    if (!input || !input.trim()) {
      showError(t('请先粘贴回调 URL'));
      return;
    }

    setLoading(true);
    try {
      // Extract code and state from the pasted callback URL
      let code = input.trim();
      let state = '';
      if (code.startsWith('http://') || code.startsWith('https://')) {
        try {
          const url = new URL(code);
          code = url.searchParams.get('code') || '';
          state = url.searchParams.get('state') || '';
        } catch {
          // Not a valid URL, use as-is
        }
      }

      if (!code) {
        showError(t('无法从 URL 中提取授权码'));
        setLoading(false);
        return;
      }

      const res = await API.post(
        '/api/channel/antigravity/oauth/complete',
        { 
          code: code,
          state: state,
        },
        { skipErrorHandler: true },
      );
      if (!res?.data?.success) {
        console.error('Antigravity OAuth complete failed:', res?.data?.message);
        throw new Error(t('授权失败'));
      }

      const key = res?.data?.data?.oauth_key || '';
      if (!key) {
        console.error('Antigravity OAuth complete response missing oauth_key:', res?.data);
        throw new Error(t('响应缺少凭据'));
      }

      onSuccess && onSuccess(key);
      showSuccess(t('已生成授权凭据'));
      onCancel && onCancel();
    } catch (error) {
      showError(error?.message || t('授权失败'));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (!visible) return;
    setAuthorizeUrl('');
    setInput('');
  }, [visible]);

  return (
    <Modal
      title={t('Antigravity 授权')}
      visible={visible}
      onCancel={onCancel}
      maskClosable={false}
      closeOnEsc
      width={720}
      footer={
        <Space>
          <Button theme='borderless' onClick={onCancel} disabled={loading}>
            {t('取消')}
          </Button>
          <Button
            theme='solid'
            type='primary'
            onClick={completeOAuth}
            loading={loading}
          >
            {t('生成并填入')}
          </Button>
        </Space>
      }
    >
      <Space vertical spacing='tight' style={{ width: '100%' }}>
        <Banner
          type='info'
          description={t(
            '1) 点击「打开授权页面」完成 Google 登录；2) 浏览器会跳转到 localhost（页面打不开也没关系）；3) 复制地址栏完整 URL 粘贴到下方；4) 点击「生成并填入」。',
          )}
        />

        <Space wrap>
          <Button type='primary' onClick={startOAuth} loading={loading}>
            {t('打开授权页面')}
          </Button>
          <Button
            theme='outline'
            disabled={!authorizeUrl || loading}
            onClick={() => copy(authorizeUrl)}
          >
            {t('复制授权链接')}
          </Button>
        </Space>

        <Input
          value={input}
          onChange={(value) => setInput(value)}
          placeholder={t('请粘贴完整回调 URL（包含 code 与 state）')}
          showClear
        />

        <Text type='tertiary' size='small'>
          {t(
            '说明：系统会自动获取 Project ID，生成结果是可直接粘贴到渠道密钥里的 JSON（包含 access_token / refresh_token / project_id）。',
          )}
        </Text>

        <Banner
          type='warning'
          description={t(
            '注意：授权凭据将加密存储在渠道密钥中。请确保你的 Google 账号已启用 Cloud Code API。',
          )}
        />
      </Space>
    </Modal>
  );
};

export default AntigravityOAuthModal;
