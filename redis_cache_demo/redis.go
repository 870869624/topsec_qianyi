package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"git.cloud.top/aiop/common/rest/config"
	"git.cloud.top/go/go-zero/core/logx"
	"git.cloud.top/ngdlp/server/pkg/model/identity"
	"github.com/redis/go-redis/v9"
)

type UserCacheService struct {
	redisClient *redis.Client
	model       identity.UserModel
}

type UserInfo struct {
	LoginName   string `json:"login_name"`
	RealName    string `json:"real_name"`
	BaseOrgName string `json:"base_org_name"`
}

// NewUserCacheService 创建用户缓存服务
func NewUserCacheService(redisConf config.Redis, model identity.UserModel) *UserCacheService {
	if redisConf.Host == "" {
		logx.Errorf("Redis配置无效: Host为空")
		return &UserCacheService{
			redisClient: nil,
			model:       model,
		}
	}

	redisClient := redis.NewClient(&redis.Options{
		Addr:     redisConf.Host,
		Password: redisConf.Pass,
		DB:       redisConf.DB,
	})

	return &UserCacheService{
		redisClient: redisClient,
		model:       model,
	}
}

// GetUserInfoByLoginName 根据登录名获取用户信息
func (s *UserCacheService) GetUserInfoByLoginName(ctx context.Context, loginName string) (*UserInfo, error) {
	if s.redisClient == nil {
		logx.Info("Redis client unavailable, querying database directly")
		dbUser, err := s.model.FindByLoginName(ctx, loginName)
		if err != nil {
			return nil, err
		}

		userInfo := &UserInfo{
			LoginName:   dbUser.LoginName,
			RealName:    dbUser.RealName,
			BaseOrgName: dbUser.BaseOrgName,
		}
		return userInfo, nil
	}

	key := s.buildLoginNameKey(loginName)

	cachedData, err := s.redisClient.Get(ctx, key).Result()
	if err == nil {
		var userInfo UserInfo
		if err := json.Unmarshal([]byte(cachedData), &userInfo); err != nil {
			logx.Errorf("Failed to unmarshal cached user info: %v", err)
			return nil, err
		}
		return &userInfo, nil
	}

	dbUser, err := s.model.FindByLoginName(ctx, loginName)
	if err != nil {
		return nil, err
	}

	userInfo := &UserInfo{
		LoginName:   dbUser.LoginName,
		RealName:    dbUser.RealName,
		BaseOrgName: dbUser.BaseOrgName,
	}

	if err := s.setUserInfoToCache(ctx, loginName, userInfo); err != nil {
		logx.Errorf("Failed to set user info to cache: %v", err)
	}

	return userInfo, nil
}

// SetUserInfoByLoginName 设置用户信息到缓存
func (s *UserCacheService) SetUserInfoByLoginName(ctx context.Context, loginName string, userInfo *UserInfo) error {
	if s.redisClient == nil {
		logx.Info("Redis client unavailable, skipping cache update")
		return nil
	}
	return s.setUserInfoToCache(ctx, loginName, userInfo)
}

// DeleteUserInfoByLoginName 从缓存中删除用户信息
func (s *UserCacheService) DeleteUserInfoByLoginName(ctx context.Context, loginName string) error {
	if s.redisClient == nil {
		logx.Info("Redis client unavailable, skipping cache deletion")
		return nil
	}
	key := s.buildLoginNameKey(loginName)
	return s.redisClient.Del(ctx, key).Err()
}

// UpdateUserInfoByLoginName 更新用户信息缓存
func (s *UserCacheService) UpdateUserInfoByLoginName(ctx context.Context, loginName string, updates map[string]interface{}) error {
	if s.redisClient == nil {
		logx.Info("Redis client unavailable, skipping cache update")
		return nil
	}

	if err := s.DeleteUserInfoByLoginName(ctx, loginName); err != nil {
		logx.Errorf("Failed to delete user cache when updating: %v", err)
	}

	if len(updates) > 0 {
		dbUser, err := s.model.FindByLoginName(ctx, loginName)
		if err != nil {
			return err
		}

		userInfo := &UserInfo{
			LoginName:   dbUser.LoginName,
			RealName:    dbUser.RealName,
			BaseOrgName: dbUser.BaseOrgName,
		}

		if err := s.setUserInfoToCache(ctx, loginName, userInfo); err != nil {
			logx.Errorf("Failed to set updated user info to cache: %v", err)
		}
	}

	return nil
}

// CreateUserCache 创建用户时设置缓存
func (s *UserCacheService) CreateUserCache(ctx context.Context, userInfo *UserInfo) error {
	if s.redisClient == nil {
		logx.Info("Redis client unavailable, skipping cache creation")
		return nil
	}
	return s.setUserInfoToCache(ctx, userInfo.LoginName, userInfo)
}

// setUserInfoToCache 内部方法：将用户信息设置到缓存
func (s *UserCacheService) setUserInfoToCache(ctx context.Context, loginName string, userInfo *UserInfo) error {
	if s.redisClient == nil {
		logx.Info("Redis client unavailable, skipping cache set operation")
		return nil
	}
	key := s.buildLoginNameKey(loginName)
	jsonData, err := json.Marshal(userInfo)
	if err != nil {
		return err
	}

	return s.redisClient.Set(ctx, key, string(jsonData), time.Duration(0)).Err()
}

// buildLoginNameKey 构建以登录名为键的Redis键
func (s *UserCacheService) buildLoginNameKey(loginName string) string {
	return fmt.Sprintf("user:login_name:%s", loginName)
}

func (s *UserCacheService) Close() error {
	if s.redisClient == nil {
		return nil
	}
	return s.redisClient.Close()
}
