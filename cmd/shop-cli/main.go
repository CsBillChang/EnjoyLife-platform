package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/CsBillChang/local-life-platform/internal/shop"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: shop-cli <shop-id>")
		os.Exit(2)
	}

	id, err := strconv.ParseInt(os.Args[1], 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, shop.ErrInvalidShopID)
		os.Exit(2)
	}

	service := shop.NewService(shop.NewMemoryRepository([]shop.Shop{
		{ID: 1, Name: "翠湖书店", Address: "五华区翠湖北路 1 号"},
		{ID: 2, Name: "南屏小馆", Address: "盘龙区南屏街 2 号"},
	}))
	result, err := service.GetByID(context.Background(), id)
	if err != nil {
		if errors.Is(err, shop.ErrInvalidShopID) || errors.Is(err, shop.ErrShopNotFound) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "shop query failed")
		os.Exit(1)
	}

	fmt.Printf("%d\t%s\t%s\n", result.ID, result.Name, result.Address)
}
