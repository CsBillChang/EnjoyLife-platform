package shop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

type MySQLRepository struct {
	db *gorm.DB
}

func NewMySQLRepository(db *gorm.DB) *MySQLRepository {
	return &MySQLRepository{db: db}
}

func (r *MySQLRepository) List(ctx context.Context, offset, limit int) ([]Shop, int, error) {
	var total int64
	if err := r.db.WithContext(ctx).Table("shops").Where("status = ?", "OPEN").Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count shops: %w", err)
	}
	var shops []Shop
	if err := r.db.WithContext(ctx).Table("shops").
		Select("id", "name", "address", "status", "version").
		Where("status = ?", "OPEN").Order("id").Limit(limit).Offset(offset).
		Find(&shops).Error; err != nil {
		return nil, 0, fmt.Errorf("list shops: %w", err)
	}
	return shops, int(total), nil
}

func (r *MySQLRepository) FindByID(ctx context.Context, id int64) (Shop, error) {
	var item Shop
	err := r.db.WithContext(ctx).Table("shops").
		Select("id", "name", "address", "status", "version").
		Where("id = ? AND status = ?", id, "OPEN").Take(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Shop{}, ErrShopNotFound
	}
	if err != nil {
		return Shop{}, fmt.Errorf("find shop: %w", err)
	}
	return item, nil
}

// UpdateOwned updates only a shop associated with the authenticated merchant.
// The membership check is part of the UPDATE predicate, so a caller cannot race
// a separate authorization read and then write another merchant's shop.
func (r *MySQLRepository) UpdateOwned(ctx context.Context, merchantID uint64, shopID int64, name, address, status string) (Shop, error) {
	var item Shop
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Table("shops").
			Where(`id = ? AND EXISTS (
			SELECT 1 FROM merchant_shop_members m
			WHERE m.shop_id = shops.id AND m.user_id = ?
		)`, shopID, merchantID).
			Updates(map[string]any{
				"name":    name,
				"address": address,
				"status":  status,
				"version": gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return fmt.Errorf("update owned shop: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrShopNotFound
		}
		if err := tx.Table("shops").Select("id", "name", "address", "status", "version").
			Where("id = ?", shopID).Take(&item).Error; err != nil {
			return fmt.Errorf("read updated shop: %w", err)
		}
		payload, err := json.Marshal(shopCacheInvalidationPayload{ShopID: item.ID, Generation: item.Version})
		if err != nil {
			return fmt.Errorf("encode shop cache invalidation: %w", err)
		}
		if err := upsertShopCacheInvalidation(tx, item.ID, item.Version, payload); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrShopNotFound) {
			return Shop{}, ErrShopNotFound
		}
		return Shop{}, fmt.Errorf("update owned shop transaction: %w", err)
	}
	return item, nil
}
